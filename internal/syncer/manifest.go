package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const stateDirName = ".icloudsync"

// Entry describes one file this tool owns on the destination.
type Entry struct {
	Path    string `json:"path"`    // actual relative path (slash separated)
	Desired string `json:"desired"` // path derived from the remote name before collision handling
	Version string `json:"version"` // etag / checksum
	Size    int64  `json:"size"`
	Dir     bool   `json:"dir,omitempty"` // an unpacked package (.app, .textbundle, ...)
}

// Manifest maps remote ids to local files. Only files listed here are ever
// deleted by the mirror logic.
type Manifest struct {
	Format    int                         `json:"format"`
	AppleID   string                      `json:"apple_id"`
	UpdatedAt time.Time                   `json:"updated_at"`
	Services  map[string]map[string]Entry `json:"services"`
}

func manifestPath(dest string) string { return filepath.Join(dest, stateDirName, "manifest.json") }

func loadManifest(dest string) (*Manifest, error) {
	m := &Manifest{Format: 1, Services: map[string]map[string]Entry{}}
	raw, err := os.ReadFile(manifestPath(dest))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("manifest %s is corrupt (move it away to rebuild): %w", manifestPath(dest), err)
	}
	if m.Services == nil {
		m.Services = map[string]map[string]Entry{}
	}
	return m, nil
}

func (m *Manifest) save(dest string) error {
	m.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dir := filepath.Join(dest, stateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "manifest-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), manifestPath(dest))
}

// lockDest takes an exclusive lock so two runs never touch the same destination.
// With create false it only locks an existing lock file (dry runs) and runs
// unlocked if there is none.
func lockDest(dest string, create bool) (release func(), err error) {
	dir := filepath.Join(dest, stateDirName)
	flags := os.O_RDONLY
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		flags = os.O_CREATE | os.O_RDWR
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), flags, 0o644)
	if !create && errors.Is(err, fs.ErrNotExist) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("another icloudsync run is using %s", dest)
		}
		// filesystem without flock support (some FUSE/network mounts): run unlocked
		return func() { f.Close() }, nil
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
