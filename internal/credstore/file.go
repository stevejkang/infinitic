package credstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevejkang/infinitic/internal/atomicfile"
)

// File keeps credentials in 0600 files: the primary at <claude config dir>/.credentials.json,
// parked accounts under poolDir.
type File struct {
	PrimaryPath string
	PoolDir     string
}

func (f File) Primary() Slot { return Slot{key: f.PrimaryPath, Label: f.PrimaryPath} }

func (f File) Pool(accountID string) Slot {
	p := filepath.Join(f.PoolDir, accountID+".json")
	return Slot{key: p, Label: p}
}

func (f File) Isolated(configDir string) Slot {
	p := filepath.Join(configDir, ".credentials.json")
	return Slot{key: p, Label: p}
}

func (File) Read(s Slot) (string, bool, error) {
	data, err := os.ReadFile(s.key)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(string(data), "\n"), true, nil
}

func (File) Write(s Slot, secret string) error {
	// atomic: a concurrent reader sees the old or the new blob, never half; symlinks are kept
	return atomicfile.WriteMode(s.key, []byte(secret), 0o600)
}

func (File) Delete(s Slot) error {
	err := os.Remove(s.key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
