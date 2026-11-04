// Package update finds infinitic releases on GitHub and installs one over the running binary.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/stevejkang/infinitic/internal/atomicfile"
)

const repo = "stevejkang/infinitic"

type Source struct {
	API      string // the repository in the GitHub API, e.g. https://api.github.com/repos/stevejkang/infinitic
	Download string // release downloads, e.g. https://github.com/stevejkang/infinitic/releases/download
	Client   *http.Client
}

func GitHub() Source {
	return Source{
		API:      "https://api.github.com/repos/" + repo,
		Download: "https://github.com/" + repo + "/releases/download",
		Client:   http.DefaultClient,
	}
}

// ErrNoRelease means the repository has no published release (prereleases do not count).
var ErrNoRelease = errors.New("no release published yet")

// Latest returns the version of the latest release, without "v". Prereleases are never latest.
func (s Source) Latest(ctx context.Context) (string, error) {
	body, err := s.get(ctx, s.API+"/releases/latest")
	if errors.Is(err, errNotFound) {
		return "", ErrNoRelease
	}
	if err != nil {
		return "", err
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("reading the latest release: %w", err)
	}
	if !Valid(rel.Tag) {
		return "", fmt.Errorf("the latest release has an unexpected tag %q", rel.Tag)
	}
	return Trim(rel.Tag), nil
}

// Install downloads release version for this OS and architecture, checks it against the
// release's checksums, and replaces the binary at exe (or the file exe links to) with it.
func (s Source) Install(ctx context.Context, version, exe string) error {
	target, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	base := fmt.Sprintf("%s/v%s/", s.Download, version)
	archive := fmt.Sprintf("infinitic_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	data, err := s.get(ctx, base+archive)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("release v%s has no %s (is the version right?)", version, archive)
	}
	if err != nil {
		return err
	}
	sums, err := s.get(ctx, base+fmt.Sprintf("infinitic_%s_checksums.txt", version))
	if err != nil {
		return fmt.Errorf("release v%s checksums: %w", version, err)
	}
	if err := verify(data, sums, archive); err != nil {
		return err
	}
	bin, err := extract(data)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	if err := atomicfile.WriteMode(target, bin, 0o755); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot replace %s: %w (run the update as the user that owns it)", target, err)
		}
		return err
	}
	return nil
}

func verify(data, sums []byte, name string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(sums), "\n") {
		if want, file, ok := strings.Cut(strings.TrimSpace(line), "  "); ok && file == name {
			if want != got {
				return fmt.Errorf("%s does not match its checksum; nothing was changed", name)
			}
			return nil
		}
	}
	return fmt.Errorf("the release checksums do not list %s; nothing was changed", name)
}

func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no infinitic binary inside")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "infinitic" {
			return io.ReadAll(tr)
		}
	}
}

var errNotFound = errors.New("not found")

func (s Source) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// Homebrew reports whether exe was installed by Homebrew, which keeps casks under Caskroom.
func Homebrew(exe string) bool {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return strings.Contains(filepath.ToSlash(exe), "/Caskroom/")
}

type Cache struct{ Path string }

type cached struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest,omitempty"`
}

// Load returns the latest version seen ("" for none) and when it was checked (zero if never).
func (c Cache) Load() (latest string, checkedAt time.Time) {
	var v cached
	data, err := os.ReadFile(c.Path)
	if err != nil || json.Unmarshal(data, &v) != nil {
		return "", time.Time{}
	}
	return v.Latest, v.CheckedAt
}

func (c Cache) Save(latest string, at time.Time) error {
	data, err := json.Marshal(cached{CheckedAt: at, Latest: latest})
	if err != nil {
		return err
	}
	return atomicfile.Write(c.Path, data, 0o600)
}
