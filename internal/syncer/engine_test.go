package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type remoteFile struct {
	key, path, content, version string
}

type fakeRemote struct {
	files   []remoteFile
	listErr error
	opens   atomic.Int32
}

func (f *fakeRemote) service(name string) Service {
	return Service{Name: name, List: func(ctx context.Context, emit func(Item) error) error {
		for _, rf := range f.files {
			rf := rf
			if err := emit(Item{
				Key: rf.key, Desired: rf.path, Version: rf.version, Size: int64(len(rf.content)),
				Open: func(context.Context) (io.ReadCloser, int64, error) {
					f.opens.Add(1)
					return io.NopCloser(strings.NewReader(rf.content)), -1, nil
				},
			}); err != nil {
				return err
			}
		}
		return f.listErr
	}}
}

func tree(t *testing.T, root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if d.Name() == stateDirName {
				return filepath.SkipDir
			}
			return nil
		}
		b, _ := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return out
}

func keys(m map[string]string) string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

func runOnce(t *testing.T, dest string, r *fakeRemote, mod func(*Options)) *Stats {
	t.Helper()
	opts := Options{Dest: dest, AppleID: "me@example.com", Workers: 3, Logf: t.Logf}
	if mod != nil {
		mod(&opts)
	}
	st, err := Run(context.Background(), opts, []Service{r.service("drive")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return st["drive"]
}

func TestMirrorLifecycle(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{
		{"1", "iCloud Drive/a.txt", "aaa", "v1"},
		{"2", "iCloud Drive/dir/b.txt", "bb", "v1"},
		{"3", "iCloud Drive/dir/sub/c.txt", "c", "v1"},
	}}
	st := runOnce(t, dest, r, nil)
	if st.Downloaded != 3 {
		t.Fatalf("downloaded %d", st.Downloaded)
	}

	// unchanged: nothing downloaded
	r.opens.Store(0)
	st = runOnce(t, dest, r, nil)
	if st.Skipped != 3 || r.opens.Load() != 0 {
		t.Fatalf("second run: %+v opens=%d", st, r.opens.Load())
	}

	// content change, rename (moved without download), deletion
	r.files = []remoteFile{
		{"1", "iCloud Drive/a.txt", "AAAA", "v2"},
		{"2", "iCloud Drive/renamed.txt", "bb", "v1"},
	}
	r.opens.Store(0)
	st = runOnce(t, dest, r, nil)
	if st.Downloaded != 1 || st.Moved != 1 || st.Deleted != 1 || r.opens.Load() != 1 {
		t.Fatalf("third run: %+v opens=%d", st, r.opens.Load())
	}
	got := tree(t, dest)
	if keys(got) != "iCloud Drive/a.txt,iCloud Drive/renamed.txt" || got["iCloud Drive/a.txt"] != "AAAA" {
		t.Fatalf("tree %v", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "iCloud Drive", "dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty directories not pruned")
	}
}

func TestUnmanagedFilesAreKept(t *testing.T) {
	dest := t.TempDir()
	os.WriteFile(filepath.Join(dest, "mine.txt"), []byte("x"), 0o644)
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "a", "v"}}}
	runOnce(t, dest, r, nil)
	r.files = nil
	r.files = []remoteFile{{"2", "iCloud Drive/b.txt", "b", "v"}}
	runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if keys(got) != "iCloud Drive/b.txt,mine.txt" {
		t.Fatalf("tree %v", got)
	}
}

func TestListingErrorPreventsDeletes(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "a", "v"}, {"2", "iCloud Drive/b.txt", "b", "v"}}}
	runOnce(t, dest, r, nil)
	r.files = r.files[:1]
	r.listErr = errors.New("network down")
	st := runOnce(t, dest, r, nil)
	if st.ListError == nil || st.Deleted != 0 {
		t.Fatalf("%+v", st)
	}
	if len(tree(t, dest)) != 2 {
		t.Fatal("file deleted despite incomplete listing")
	}
}

func TestEmptyListingBlocked(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "a", "v"}}}
	runOnce(t, dest, r, nil)
	r.files = nil
	st := runOnce(t, dest, r, nil)
	if st.DeletionsBlocked == "" || len(tree(t, dest)) != 1 {
		t.Fatalf("empty listing should not wipe the mirror: %+v", st)
	}
	// --force-delete lifts the percentage threshold but never the empty-listing guard
	st = runOnce(t, dest, r, func(o *Options) { o.ForceDelete = true })
	if st.Deleted != 0 || len(tree(t, dest)) != 1 {
		t.Fatalf("force delete wiped the mirror on an empty listing: %+v", st)
	}
}

func TestMassDeletionThreshold(t *testing.T) {
	dest := t.TempDir()
	var files []remoteFile
	for i := 0; i < 100; i++ {
		files = append(files, remoteFile{fmt.Sprint(i), fmt.Sprintf("D/f%d.txt", i), "x", "v"})
	}
	r := &fakeRemote{files: files}
	runOnce(t, dest, r, nil)
	// 60 of 100 vanish while 100 new files appear: the threshold is measured
	// against the files that were there before the run
	r.files = files[:40]
	for i := 100; i < 200; i++ {
		r.files = append(r.files, remoteFile{fmt.Sprint(i), fmt.Sprintf("D/f%d.txt", i), "y", "v"})
	}
	st := runOnce(t, dest, r, nil)
	if st.DeletionsBlocked == "" || st.Deleted != 0 || len(tree(t, dest)) != 200 {
		t.Fatalf("mass deletion not blocked: %+v", st)
	}
	st = runOnce(t, dest, r, func(o *Options) { o.ForceDelete = true })
	if st.Deleted != 60 || len(tree(t, dest)) != 140 {
		t.Fatalf("force delete: %+v", st)
	}
}

func TestCaseCollisionGetsSuffix(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{
		{"1", "Photos/2024/01/01/IMG_1.JPG", "one", "v"},
		{"2", "Photos/2024/01/01/img_1.jpg", "two", "v"},
	}}
	runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if got["Photos/2024/01/01/IMG_1.JPG"] != "one" || got["Photos/2024/01/01/img_1 (2).jpg"] != "two" {
		t.Fatalf("tree %v", got)
	}
	// stable on re-run
	st := runOnce(t, dest, r, nil)
	if st.Skipped != 2 {
		t.Fatalf("%+v", st)
	}
}

func TestResumeAdoptsFilesWithoutManifest(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "abc", "v"}}}
	runOnce(t, dest, r, nil)
	os.Remove(manifestPath(dest))
	r.opens.Store(0)
	st := runOnce(t, dest, r, nil)
	if st.Adopted != 1 || r.opens.Load() != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "abc", "v"}}}
	st := runOnce(t, dest, r, func(o *Options) { o.DryRun = true })
	if len(tree(t, dest)) != 0 || r.opens.Load() != 0 || st.Listed != 1 {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(manifestPath(dest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manifest written in dry run")
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("dry run created %v", entries)
	}
}

func TestRefusesOtherAccountAndMissingDest(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "a", "v"}}}
	runOnce(t, dest, r, nil)
	_, err := Run(context.Background(), Options{Dest: dest, AppleID: "other@example.com"}, []Service{r.service("drive")})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("want refusal, got %v", err)
	}
	_, err = Run(context.Background(), Options{Dest: filepath.Join(dest, "nope")}, nil)
	if !errors.Is(err, ErrDestMissing) {
		t.Fatalf("want ErrDestMissing, got %v", err)
	}
}

func TestShortDownloadFails(t *testing.T) {
	dest := t.TempDir()
	svc := Service{Name: "drive", List: func(ctx context.Context, emit func(Item) error) error {
		return emit(Item{Key: "1", Desired: "iCloud Drive/x", Version: "v", Size: 10,
			Open: func(context.Context) (io.ReadCloser, int64, error) {
				return io.NopCloser(strings.NewReader("abc")), -1, nil
			}})
	}}
	st, err := Run(context.Background(), Options{Dest: dest, Workers: 1}, []Service{svc})
	if err != nil {
		t.Fatal(err)
	}
	if st["drive"].Failed != 1 || len(tree(t, dest)) != 0 {
		t.Fatalf("%+v %v", st["drive"], tree(t, dest))
	}
}

func TestSanitizeAndSafeJoin(t *testing.T) {
	cases := map[string]string{
		"a/b":       "a_b",
		"..":        "_",
		"name. ":    "name",
		`x:y*?"<>|`: "x_y______",
		"":          "_",
		"é":        "é", // NFD -> NFC
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	if len(SanitizeName(strings.Repeat("я", 300)+".jpg")) > maxNameBytes {
		t.Error("long name not truncated")
	}
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "a//b", ""} {
		if _, err := safeJoin("/dest", bad); err == nil {
			t.Errorf("safeJoin accepted %q", bad)
		}
	}
}

func TestReplacedFileDropsSuffix(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/a.txt", "old", "v"}}}
	runOnce(t, dest, r, nil)
	// deleted and recreated in iCloud: same name, new id
	r.files = []remoteFile{{"2", "iCloud Drive/a.txt", "new", "v"}}
	runOnce(t, dest, r, nil)
	runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if keys(got) != "iCloud Drive/a.txt" || got["iCloud Drive/a.txt"] != "new" {
		t.Fatalf("tree %v", got)
	}
}

func TestRenameAndNewFileWithOldName(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "iCloud Drive/A.txt", "first", "v"}}}
	runOnce(t, dest, r, nil)
	r.files = []remoteFile{
		{"3", "iCloud Drive/A.txt", "second", "v"},
		{"1", "iCloud Drive/B.txt", "first", "v"},
	}
	runOnce(t, dest, r, nil)
	runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if keys(got) != "iCloud Drive/A.txt,iCloud Drive/B.txt" || got["iCloud Drive/A.txt"] != "second" || got["iCloud Drive/B.txt"] != "first" {
		t.Fatalf("tree %v", got)
	}
}

func TestPackageSizeDiffersFromListing(t *testing.T) {
	dest := t.TempDir()
	opens := 0
	svc := Service{Name: "drive", List: func(ctx context.Context, emit func(Item) error) error {
		return emit(Item{Key: "1", Desired: "iCloud Drive/doc.pages", Version: "v", Size: 999,
			Open: func(context.Context) (io.ReadCloser, int64, error) {
				opens++
				return io.NopCloser(strings.NewReader("zipdata")), 7, nil
			}})
	}}
	for i := 0; i < 2; i++ {
		st, err := Run(context.Background(), Options{Dest: dest, Workers: 1}, []Service{svc})
		if err != nil || st["drive"].Failed != 0 {
			t.Fatalf("run %d: %v %+v", i, err, st["drive"])
		}
	}
	if opens != 1 {
		t.Fatalf("package re-downloaded: opens=%d", opens)
	}
}

func TestCaseOnlyRenameWithChangeKeepsFile(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "D/Photo.jpg", "v1", "1"}}}
	runOnce(t, dest, r, nil)
	r.files = []remoteFile{{"1", "D/photo.jpg", "v2!", "2"}}
	st := runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if len(got) != 1 || got["D/photo.jpg"] != "v2!" || st.Failed != 0 {
		t.Fatalf("tree %v stats %+v", got, st)
	}
	// and the next run is a no-op
	if st := runOnce(t, dest, r, nil); st.Skipped != 1 || st.Downloaded != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestCaseOnlyMoveKeepsReservation(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "D/Photo.jpg", "one", "1"}}}
	runOnce(t, dest, r, nil)
	// item 1 changes case only (a move), and a new item wants the same name
	r.files = []remoteFile{{"1", "D/photo.jpg", "one", "1"}, {"2", "D/PHOTO.jpg", "two", "1"}}
	st := runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if len(got) != 2 || got["D/photo.jpg"] != "one" || got["D/PHOTO (2).jpg"] != "two" {
		t.Fatalf("tree %v stats %+v", got, st)
	}
	if st := runOnce(t, dest, r, nil); st.Skipped != 2 {
		t.Fatalf("not stable: %+v", st)
	}
}

func TestUnmanagedFilesAreNeverOverwritten(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "D"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a user's own file at the path an iCloud item wants (different size)
	if err := os.WriteFile(filepath.Join(dest, "D", "a.txt"), []byte("USER DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fakeRemote{files: []remoteFile{{"1", "D/a.txt", "icloud", "1"}, {"2", "D/b.txt", "bee", "1"}}}
	runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if got["D/a.txt"] != "USER DATA" || got["D/a (2).txt"] != "icloud" {
		t.Fatalf("tree %v", got)
	}
	// a move onto an unmanaged file must not clobber it either
	if err := os.WriteFile(filepath.Join(dest, "D", "c.txt"), []byte("USER C"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.files[1].path = "D/c.txt"
	st := runOnce(t, dest, r, nil)
	got = tree(t, dest)
	if got["D/c.txt"] != "USER C" || got["D/c (2).txt"] != "bee" || st.Failed != 0 {
		t.Fatalf("tree %v stats %+v", got, st)
	}
}

func TestStaleTempFilesAreRemoved(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "Drive/a.txt", "a", "v"}}}
	runOnce(t, dest, r, nil)
	stale := filepath.Join(dest, "Drive", ".icloudsync-123.part")
	mine := filepath.Join(dest, "Drive", "user.part")
	for _, p := range []string{stale, mine} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runOnce(t, dest, r, nil)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temp file kept")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("unrelated file removed")
	}
}

func TestProgressIsReported(t *testing.T) {
	old := ProgressInterval
	ProgressInterval = 10 * time.Millisecond
	defer func() { ProgressInterval = old }()
	var mu sync.Mutex
	var lines []string
	slow := Service{Name: "drive", List: func(ctx context.Context, emit func(Item) error) error {
		time.Sleep(60 * time.Millisecond)
		return nil
	}}
	_, err := Run(context.Background(), Options{Dest: t.TempDir(), Logf: func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}}, []Service{slow})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "[drive] progress: listed 0") {
			return
		}
	}
	t.Fatalf("no progress line in %q", lines)
}

func TestConcurrentRunIsRefused(t *testing.T) {
	dest := t.TempDir()
	release, err := lockDest(dest, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = Run(context.Background(), Options{Dest: dest}, nil)
	if err == nil || !strings.Contains(err.Error(), "another icloudsync run") {
		t.Fatalf("err = %v", err)
	}
}

func TestFoldKeyUsesUnicodeFolding(t *testing.T) {
	// final sigma and German sharp s fold like a case-insensitive disk does
	if foldKey("ΟΔΟΣ.txt") != foldKey("οδος.txt") || foldKey("Café") != foldKey("CAFE\u0301") {
		t.Fatal("folding misses equivalent names")
	}
}

// A run killed after moving/re-downloading files but before saving the
// manifest: the next run must pick the files up, not duplicate them.
func TestKilledRunWorkIsAdoptedNotDuplicated(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{{"1", "D/old/a.txt", "aaa", "v1"}, {"2", "D/old/b.txt", "bbb", "v1"}}}
	runOnce(t, dest, r, nil)
	// what the killed run did on disk: moved a.txt, re-downloaded b.txt (v2) at its new path
	if err := os.MkdirAll(filepath.Join(dest, "D", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dest, "D/old/a.txt"), filepath.Join(dest, "D/new/a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "D/new/b.txt"), []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dest, "D/old/b.txt"))
	r.files = []remoteFile{{"1", "D/new/a.txt", "aaa", "v1"}, {"2", "D/new/b.txt", "bbbb", "v2"}}
	opens := r.opens.Load()
	st := runOnce(t, dest, r, nil)
	got := tree(t, dest)
	if len(got) != 2 || got["D/new/a.txt"] != "aaa" || got["D/new/b.txt"] != "bbbb" || r.opens.Load() != opens || st.Adopted != 2 {
		t.Fatalf("tree %v stats %+v opens %d", got, st, r.opens.Load()-opens)
	}
	if st := runOnce(t, dest, r, nil); st.Skipped != 2 {
		t.Fatalf("not stable: %+v", st)
	}
}

func trashFiles(t *testing.T, dest string) map[string]string {
	t.Helper()
	root := filepath.Join(dest, stateDirName, trashDirName)
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2) // drop the batch folder
		b, _ := os.ReadFile(p)
		out[parts[1]] = string(b)
		return nil
	})
	return out
}

func TestTrashKeepsDeletedFiles(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{
		{"1", "iCloud Drive/a.txt", "aaa", "v1"},
		{"2", "iCloud Drive/dir/b.txt", "bb", "v1"},
	}}
	runOnce(t, dest, r, nil)
	r.files = r.files[:1]

	// a dry run neither deletes nor creates the trash
	runOnce(t, dest, r, func(o *Options) { o.DryRun = true; o.Trash = true })
	if _, err := os.Stat(filepath.Join(dest, stateDirName, trashDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a dry run created the trash")
	}
	if got := tree(t, dest); len(got) != 2 {
		t.Fatalf("dry run changed the tree: %v", got)
	}

	st := runOnce(t, dest, r, func(o *Options) { o.Trash = true })
	if st.Deleted != 1 {
		t.Fatalf("%+v", st)
	}
	if got := tree(t, dest); keys(got) != "iCloud Drive/a.txt" {
		t.Fatalf("tree %v", got)
	}
	if got := trashFiles(t, dest); got["iCloud Drive/dir/b.txt"] != "bb" {
		t.Fatalf("trash %v", got)
	}
	// the manifest forgot it, so a later run does not try again
	if st := runOnce(t, dest, r, func(o *Options) { o.Trash = true }); st.Deleted != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestTrashStillHonoursSafetyChecks(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{}
	for i := 0; i < 200; i++ {
		r.files = append(r.files, remoteFile{fmt.Sprint(i), fmt.Sprintf("iCloud Drive/f%03d.txt", i), "x", "v"})
	}
	runOnce(t, dest, r, nil)
	r.files = r.files[:100] // half would go: more than 25%
	st := runOnce(t, dest, r, func(o *Options) { o.Trash = true })
	if st.Deleted != 0 || st.DeletionsBlocked == "" {
		t.Fatalf("mass deletion went through the trash: %+v", st)
	}
	if len(trashFiles(t, dest)) != 0 {
		t.Fatal("trash filled despite the safeguard")
	}
}

func TestPurgeTrashOnlyTouchesOldBatches(t *testing.T) {
	dest := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.Local)
	mk := func(name string) string {
		p := filepath.Join(trashRoot(dest), name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o644)
		return p
	}
	old := mk(trashStampFor(now.AddDate(0, 0, -40)))
	recent := mk(trashStampFor(now.AddDate(0, 0, -5)))
	other := mk("my-notes") // not a batch stamp: never touched
	purgeTrash(dest, 30, now, t.Logf)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old batch survived")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was purged", p)
		}
	}
	purgeTrash(dest, 0, now, t.Logf) // 0 = keep forever
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("days=0 purged something")
	}
}

func TestMoveToTrashDoesNotOverwrite(t *testing.T) {
	dest := t.TempDir()
	for i, body := range []string{"one", "two"} {
		abs := filepath.Join(dest, "iCloud Drive", "same.txt")
		_ = os.MkdirAll(filepath.Dir(abs), 0o755)
		_ = os.WriteFile(abs, []byte(body), 0o644)
		if err := moveToTrash(dest, "20261122-120000", "iCloud Drive/same.txt", abs); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	got := trashFiles(t, dest)
	if got["iCloud Drive/same.txt"] != "one" || got["iCloud Drive/same (2).txt"] != "two" {
		t.Fatalf("%v", got)
	}
}
