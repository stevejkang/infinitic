package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.5.0", "0.4.0", true},
		{"v0.4.1", "0.4.0", true},
		{"1.0.0", "0.9.9", true},
		{"0.10.0", "0.9.0", true},
		{"0.4.0", "0.4.0-rc1", true},
		{"0.4.0-rc2", "0.4.0-rc1", true},
		{"0.4.0-rc10", "0.4.0-rc2", true},
		{"0.4.0", "0.4.0", false},
		{"0.4.0-rc1", "0.4.0", false},
		{"0.3.9", "0.4.0-rc1", false},
		{"dev", "0.4.0", false},
		{"0.4.0", "dev", false},
		{"1.2", "1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func archive(t *testing.T, bin string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"LICENSE": "mit", "infinitic": bin} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// server serves one release; sumOf overrides the checksum it lists.
func server(t *testing.T, version string, data []byte, sumOf string) Source {
	t.Helper()
	name := fmt.Sprintf("infinitic_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(data)
	listed := hex.EncodeToString(sum[:])
	if sumOf != "" {
		listed = sumOf
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v%s","prerelease":false}`, version)
		case "/dl/v" + version + "/" + name:
			_, _ = w.Write(data)
		case "/dl/v" + version + "/infinitic_" + version + "_checksums.txt":
			fmt.Fprintf(w, "%s  %s\nffff  other.tar.gz\n", listed, name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return Source{API: srv.URL + "/api", Download: srv.URL + "/dl", Client: srv.Client()}
}

func TestLatest(t *testing.T) {
	s := server(t, "0.5.0", nil, "")
	if v, err := s.Latest(context.Background()); err != nil || v != "0.5.0" {
		t.Fatalf("got %q %v", v, err)
	}
	none := Source{API: s.API + "/missing", Client: s.Client}
	if _, err := none.Latest(context.Background()); err != ErrNoRelease {
		t.Fatalf("no release: got %v", err)
	}
}

func TestInstallReplacesTheBinaryBehindASymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "Cellar", "infinitic")
	link := filepath.Join(dir, "bin", "infinitic")
	for _, err := range []error{
		os.MkdirAll(filepath.Dir(real), 0o755),
		os.MkdirAll(filepath.Dir(link), 0o755),
		os.WriteFile(real, []byte("old"), 0o700),
		os.Symlink(real, link),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	s := server(t, "0.5.0", archive(t, "new binary"), "")
	if err := s.Install(context.Background(), "0.5.0", link); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(real)
	info, _ := os.Stat(real)
	if string(got) != "new binary" || info.Mode().Perm() != 0o755 {
		t.Fatalf("binary %q mode %v", got, info.Mode().Perm())
	}
	if l, err := os.Lstat(link); err != nil || l.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
}

func TestInstallRefusesABadChecksumOrAMissingVersion(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "infinitic")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := server(t, "0.5.0", archive(t, "evil"), strings.Repeat("0", 64))
	if err := bad.Install(context.Background(), "0.5.0", exe); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if err := bad.Install(context.Background(), "9.9.9", exe); err == nil || !strings.Contains(err.Error(), "v9.9.9") {
		t.Fatalf("missing version: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("binary changed to %q", got)
	}
}

func TestHomebrew(t *testing.T) {
	if !Homebrew("/opt/homebrew/Caskroom/infinitic/0.4.0/infinitic") || Homebrew("/home/me/.local/bin/infinitic") {
		t.Fatal("Homebrew detection")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := Cache{Path: filepath.Join(t.TempDir(), "update.json")}
	if v, at := c.Load(); v != "" || !at.IsZero() {
		t.Fatal("empty cache")
	}
	now := time.Now().Truncate(time.Second)
	if err := c.Save("0.5.0", now); err != nil {
		t.Fatal(err)
	}
	if v, at := c.Load(); v != "0.5.0" || !at.Equal(now) {
		t.Fatalf("got %q %v", v, at)
	}
}
