package syncer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	trashDirName     = "trash"
	trashStampLayout = "20060102-150405"
)

// trashRoot is <dest>/.icloudsync/trash.
func trashRoot(dest string) string { return filepath.Join(dest, stateDirName, trashDirName) }

// moveToTrash moves a mirrored file (or unpacked package) that disappeared
// from iCloud into <dest>/.icloudsync/trash/<stamp>/<rel> instead of deleting
// it. The trash is on the same file system, so this is a rename, not a copy.
func moveToTrash(dest, stamp, rel, abs string) error {
	target, err := safeJoin(filepath.Join(trashRoot(dest), stamp), rel)
	if err != nil {
		return err
	}
	// never overwrite an earlier trashed copy from the same run
	for n := 2; ; n++ {
		if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return err
		}
		target, err = safeJoin(filepath.Join(trashRoot(dest), stamp), withSuffix(rel, n))
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.Rename(abs, target)
}

// purgeTrash deletes trash batches older than days. Only directories named
// like a batch stamp are touched, nothing else in the trash folder.
func purgeTrash(dest string, days int, now time.Time, logf func(string, ...any)) {
	if days <= 0 {
		return
	}
	entries, err := os.ReadDir(trashRoot(dest))
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -days)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := time.ParseInLocation(trashStampLayout, e.Name(), time.Local)
		if err != nil || !t.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(trashRoot(dest), e.Name())); err != nil {
			logf("could not purge trash batch %s: %v", e.Name(), err)
			continue
		}
		logf("purged trash batch %s (older than %d days)", e.Name(), days)
	}
}

func trashStampFor(t time.Time) string { return t.Format(trashStampLayout) }
