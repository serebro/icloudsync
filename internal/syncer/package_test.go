package syncer

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/serebro/icloudsync/internal/icloud"
)

func zipOf(t *testing.T, files map[string]string, links map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	for name, target := range links {
		h := &zip.FileHeader{Name: name}
		h.SetMode(os.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, target)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// a Drive with one empty file and one package, served like Apple does
type fakeDrivePkg struct {
	mu      sync.Mutex
	pkgZip  []byte
	etag    string
	hits    map[string]int
	present bool
}

func (f *fakeDrivePkg) server(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /retrieveItemDetailsInFolders", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		pkg := ""
		if f.present {
			pkg = fmt.Sprintf(`,{"drivewsid":"FILE::z::P","docwsid":"P","zone":"com.apple.CloudDocs","name":"Notes","extension":"textbundle","type":"FILE","size":999999,"etag":%q}`, f.etag)
		}
		fmt.Fprintf(w, `[{"drivewsid":"%s","items":[
			{"drivewsid":"FILE::z::E","docwsid":"E","zone":"com.apple.CloudDocs","name":".localized","type":"FILE","size":0,"etag":"e"}%s]}]`, icloud.DriveRootID, pkg)
	})
	mux.HandleFunc("GET /ws/com.apple.CloudDocs/download/by_id", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("document_id")
		f.mu.Lock()
		f.hits["by_id:"+id]++
		f.mu.Unlock()
		if id != "P" {
			fmt.Fprintf(w, `{"data_token":{"url":"%s/content/%s"}}`, srv.URL, id)
			return
		}
		fmt.Fprintf(w, `{"package_token":{"url":"%s/content/P"},"manifest_token":{"url":"%s/manifest"}}`, srv.URL, srv.URL)
	})
	mux.HandleFunc("GET /content/E", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // what Apple does for empty files
	})
	mux.HandleFunc("GET /content/P", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b := f.pkgZip
		f.mu.Unlock()
		w.Write(b) // chunked: no Content-Length, size differs from the listing
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDrivePackagesAndEmptyFiles(t *testing.T) {
	f := &fakeDrivePkg{etag: "v1", hits: map[string]int{}, present: true,
		pkgZip: zipOf(t, map[string]string{"E34D-INTERNAL.textbundle/text.md": "# hi", "E34D-INTERNAL.textbundle/assets/a.png": "png"}, map[string]string{"E34D-INTERNAL.textbundle/link.md": "text.md"})}
	srv := f.server(t)
	c := icloud.NewTestClient(t.TempDir(), "42", map[string]string{"drivews": srv.URL, "docws": srv.URL}, nil)
	dest := t.TempDir()
	st := runServices(t, dest, DriveService(c))["drive"]
	if st.Failed != 0 || st.Downloaded != 2 {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	if got["Drive/.localized"] != "" || got["Drive/Notes.textbundle/text.md"] != "# hi" || got["Drive/Notes.textbundle/assets/a.png"] != "png" {
		t.Fatalf("tree %v", got)
	}
	if _, ok := got["Drive/.localized"]; !ok {
		t.Fatal("empty file not created")
	}
	if f.hits["by_id:E"] != 0 {
		t.Fatal("empty file was requested from Apple")
	}
	if l, err := os.Readlink(filepath.Join(dest, "Drive/Notes.textbundle/link.md")); err != nil || l != "text.md" {
		t.Fatalf("symlink %q %v", l, err)
	}

	// unchanged: nothing is downloaded again
	if st := runServices(t, dest, DriveService(c))["drive"]; st.Skipped != 2 || st.Downloaded != 0 {
		t.Fatalf("second run %+v", st)
	}

	// changed package: replaced as a whole, stale files inside it disappear
	f.mu.Lock()
	f.etag = "v2"
	f.pkgZip = zipOf(t, map[string]string{"Notes.textbundle/text.md": "# v2"}, nil) // inner name may also match
	f.mu.Unlock()
	if st := runServices(t, dest, DriveService(c))["drive"]; st.Downloaded != 1 || st.Failed != 0 {
		t.Fatalf("update %+v", st)
	}
	got = tree(t, dest)
	if got["Drive/Notes.textbundle/text.md"] != "# v2" || len(got) != 2 {
		t.Fatalf("after update %v", got)
	}

	// removed from iCloud: the package directory is deleted
	f.mu.Lock()
	f.present = false
	f.mu.Unlock()
	if st := runServices(t, dest, DriveService(c))["drive"]; st.Deleted != 1 {
		t.Fatalf("delete %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dest, "Drive/Notes.textbundle")); !os.IsNotExist(err) {
		t.Fatal("package directory kept")
	}
}

func TestUnzipRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	for name, z := range map[string][]byte{
		"dotdot":   zipOf(t, map[string]string{"../evil": "x"}, nil),
		"absolute": zipOf(t, map[string]string{"/tmp/evil": "x"}, nil),
	} {
		p := filepath.Join(dir, name+".zip")
		os.WriteFile(p, z, 0o644)
		root := filepath.Join(dir, name)
		os.MkdirAll(root, 0o755)
		if err := unzipInto(p, root); err == nil {
			t.Errorf("%s: unsafe archive accepted", name)
		}
	}
	// a symlink pointing outside is skipped, one inside is kept
	p := filepath.Join(dir, "links.zip")
	os.WriteFile(p, zipOf(t, map[string]string{"a/f": "x"}, map[string]string{"a/out": "../../../etc/passwd", "a/abs": "/etc/passwd", "a/in": "f"}), 0o644)
	root := filepath.Join(dir, "links")
	os.MkdirAll(root, 0o755)
	if err := unzipInto(p, root); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"out", "abs"} {
		if _, err := os.Lstat(filepath.Join(root, "a", n)); !os.IsNotExist(err) {
			t.Errorf("escaping symlink %s created", n)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "a", "in")); err != nil {
		t.Error("inner symlink missing")
	}
	if entries, _ := os.ReadDir(dir); len(entries) == 0 || strings.Contains(fmt.Sprint(entries), "evil") {
		t.Fatalf("escape happened: %v", entries)
	}
	_ = context.Background
}

func TestNestedPackageFromOlderVersionIsRepaired(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "Drive/x.txt", "x", "v"}}}
	runOnce(t, dest, r, nil)
	// what the previous version left: Drive/N.textbundle/INNER.textbundle/text.md
	pkg := filepath.Join(dest, "Drive", "N.textbundle")
	if err := os.MkdirAll(filepath.Join(pkg, "INNER.textbundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pkg, "INNER.textbundle", "text.md"), []byte("t"), 0o644)
	os.WriteFile(filepath.Join(pkg, "._INNER.textbundle"), []byte("appledouble"), 0o644)
	m, err := loadManifest(dest)
	if err != nil {
		t.Fatal(err)
	}
	m.Services["drive"]["2"] = Entry{Path: "Drive/N.textbundle", Desired: "Drive/N.textbundle", Version: "p", Size: 10, Dir: true}
	if err := m.save(dest); err != nil {
		t.Fatal(err)
	}
	r.files = append(r.files, remoteFile{"2", "Drive/N.textbundle", "zip", "p"})
	st := runOnce(t, dest, r, nil)
	if st.Skipped != 2 || st.Downloaded != 0 {
		t.Fatalf("%+v", st)
	}
	if b, err := os.ReadFile(filepath.Join(pkg, "text.md")); err != nil || string(b) != "t" {
		t.Fatalf("not flattened: %v", tree(t, dest))
	}
	// a package that legitimately contains one folder of another kind is left alone
	if fixed, _ := flattenPackage(filepath.Join(dest, "Drive")); fixed {
		t.Fatal("non-package flattened")
	}
}
