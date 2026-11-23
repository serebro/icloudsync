package syncer

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const maxNameBytes = 240 // leave room for collision suffixes under the usual 255 limit

// SanitizeName makes one remote path component safe on APFS, exFAT, FAT32 and NTFS.
func SanitizeName(s string) string {
	s = norm.NFC.String(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s = strings.TrimLeft(strings.TrimRight(b.String(), " ."), " ")
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	if len(s) > maxNameBytes {
		ext := path.Ext(s)
		if len(ext) > 16 {
			ext = ""
		}
		stem := s[:maxNameBytes-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		s = stem + ext
	}
	return s
}

// foldKey is used to detect paths that collide on case-insensitive or
// normalization-insensitive filesystems.
// foldKey is how case-insensitive file systems compare names: Unicode case
// folding of the NFC form.
func foldKey(rel string) string { return cases.Fold().String(norm.NFC.String(rel)) }

// withSuffix turns "a/b/name.ext" into "a/b/name (n).ext".
func withSuffix(rel string, n int) string {
	dir, file := path.Split(rel)
	ext := path.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	return dir + fmt.Sprintf("%s (%d)%s", stem, n, ext)
}

// safeJoin joins a slash-separated relative path onto root and makes sure
// the result stays inside root.
func safeJoin(root, rel string) (string, error) {
	if rel == "" || path.IsAbs(rel) {
		return "", fmt.Errorf("invalid relative path %q", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid relative path %q", rel)
		}
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes destination", rel)
	}
	return abs, nil
}

func fileSize(p string) (int64, bool) {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	return fi.Size(), true
}

// pruneEmptyDirs removes empty directories from dir upwards, stopping at stop.
func pruneEmptyDirs(dir, stop string) {
	for {
		if dir == stop || len(dir) <= len(stop) || !strings.HasPrefix(dir, stop) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.Name() != ".DS_Store" {
				return
			}
		}
		_ = os.Remove(filepath.Join(dir, ".DS_Store"))
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// explainFSError adds a human hint for common external-drive failures.
func explainFSError(err error) error {
	switch {
	case errors.Is(err, syscall.EFBIG):
		return fmt.Errorf("%w (file too large for the destination filesystem; FAT32 is limited to 4 GB per file, use exFAT/APFS)", err)
	case errors.Is(err, syscall.ENOSPC):
		return fmt.Errorf("%w (destination is full)", err)
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("%w (destination is read-only)", err)
	}
	return err
}

func isFatalFSError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EIO)
}
