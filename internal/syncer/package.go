package syncer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func isZipFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	_, err = io.ReadFull(f, head)
	return err == nil && bytes.Equal(head, []byte("PK\x03\x04"))
}

// installPackage unpacks the zip archive at zipPath into a directory at abs,
// replacing what is there. It unpacks next to abs first and swaps the result
// in, so an interrupted run never leaves a half-written package at abs.
func installPackage(zipPath, abs string) error {
	dir := filepath.Dir(abs)
	tmp, err := os.MkdirTemp(dir, ".icloudsync-*.part.d")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := unzipInto(zipPath, tmp); err != nil {
		return fmt.Errorf("unpacking package: %w", err)
	}
	src := tmp
	if inner, ok := wrappedPackage(tmp, abs); ok {
		src = inner
	}
	var old string
	if _, err := os.Lstat(abs); err == nil {
		old = filepath.Join(tmp, ".previous")
		if err := os.Rename(abs, old); err != nil {
			return err
		}
	}
	if err := os.Rename(src, abs); err != nil {
		if old != "" {
			_ = os.Rename(old, abs)
		}
		return err
	}
	return nil
}

// wrappedPackage: Apple's archive holds the package folder itself, under its
// original (often internal) name, e.g. "E34D....textbundle/text.md". If dir
// contains just one folder with the same extension as abs, that folder is the
// package. AppleDouble "._*" files and .DS_Store (exFAT/FAT metadata) are ignored.
func wrappedPackage(dir, abs string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var only os.DirEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "._") || e.Name() == ".DS_Store" {
			continue
		}
		if only != nil {
			return "", false
		}
		only = e
	}
	if only == nil || !only.IsDir() || !strings.EqualFold(filepath.Ext(only.Name()), filepath.Ext(abs)) || filepath.Ext(abs) == "" {
		return "", false
	}
	return filepath.Join(dir, only.Name()), true
}

// flattenPackage repairs a package unpacked by an earlier version as
// abs/<inner>.ext/...: the inner folder replaces abs. Reports whether it did.
func flattenPackage(abs string) (bool, error) {
	inner, ok := wrappedPackage(abs, abs)
	if !ok {
		return false, nil
	}
	tmp := filepath.Join(filepath.Dir(abs), fmt.Sprintf(".icloudsync-flatten-%d.part.d", os.Getpid()))
	if err := os.Rename(inner, tmp); err != nil {
		return false, err
	}
	if err := os.RemoveAll(abs); err != nil { // only metadata files are left in it
		return false, err
	}
	return true, os.Rename(tmp, abs)
}

// unzipInto extracts an archive below root. Entries that would land outside
// root are refused; symlinks are created last and only if they point inside.
func unzipInto(zipPath, root string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	type link struct{ at, target string }
	var links []link
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in archive", f.Name)
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			links = append(links, link{dst, string(target)})
		default:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := extractFile(f, dst, mode.Perm()|0o600); err != nil {
				return err
			}
		}
	}
	for _, l := range links {
		if filepath.IsAbs(l.target) {
			continue // would point outside the package
		}
		resolved := filepath.Join(filepath.Dir(l.at), l.target)
		if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(l.at), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(l.target, l.at); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, dst string, perm os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
