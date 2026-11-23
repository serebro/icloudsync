package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/serebro/icloudsync/internal/icloud"
)

// Item is one remote file.
type Item struct {
	Key     string // stable remote id
	Group   string // items of one asset share a group (a photo and its Live video); "" = alone
	Desired string // slash-separated relative path built from sanitized names
	Version string // changes when content changes (etag / checksum)
	Size    int64  // listed size; packages may download with a different size
	ModTime time.Time
	// Open returns the content and its length (-1 when unknown).
	Open func(ctx context.Context) (io.ReadCloser, int64, error)
}

// Lister streams every remote item to emit. It must return nil only when the
// listing is complete: mirror deletions depend on that.
type Lister func(ctx context.Context, emit func(Item) error) error

// ErrStopListing ends a listing early and cleanly. A Lister returns it when it
// has seen enough (--recent), and emit returns it when --until-found is
// satisfied. The service must have set Partial: nothing is deleted after it.
var ErrStopListing = errors.New("listing stopped early")

type Service struct {
	Name string // manifest section, e.g. "drive"
	List Lister
	// Partial marks a listing that is incomplete on purpose (--recent,
	// --until-found): it is not an error, but "not listed" no longer means
	// "gone", so nothing is deleted.
	Partial bool
	// UntilFound > 0 stops the listing after that many consecutive groups whose
	// files were all already up to date.
	UntilFound int
}

type Options struct {
	Dest        string
	AppleID     string
	DryRun      bool
	NoDelete    bool
	ForceDelete bool
	// Trash moves files that disappeared from iCloud into .icloudsync/trash/<time>/
	// instead of deleting them; TrashDays > 0 purges batches older than that.
	Trash     bool
	TrashDays int
	Workers   int
	Logf      func(format string, a ...any)
}

type Stats struct {
	Listed, Downloaded, Moved, Adopted, Skipped, Deleted, Failed int
	Bytes                                                        int64
	ListError                                                    error
	DeletionsBlocked                                             string
	Partial                                                      bool // listed on purpose in part: nothing was deleted
	StoppedEarly                                                 bool // --until-found / --recent ended the listing
}

var ErrDestMissing = errors.New("destination does not exist")

type engine struct {
	opts     Options
	man      *Manifest
	mu       sync.Mutex // guards man, per-service state and stats
	lastSave time.Time
	fatalErr error
	cancel   context.CancelFunc
	trashAt  string // batch folder name for this run's trashed files
}

type svcState struct {
	e        *engine
	name     string
	entries  map[string]Entry
	reserved map[string]string // foldKey(path) -> key
	seen     map[string]bool
	prior    int // manifest entries before this run (base for the deletion threshold)
	stats    *Stats

	// --until-found bookkeeping, touched only by the listing goroutine
	group         string
	groupSeen     bool
	groupUpToDate bool
	foundRun      int
}

type job struct {
	it      Item
	rel     string
	prev    Entry
	hadPrev bool
}

// Run syncs all services into opts.Dest.
func Run(ctx context.Context, opts Options, services []Service) (map[string]*Stats, error) {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	fi, err := os.Stat(opts.Dest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (is the drive mounted?)", ErrDestMissing, opts.Dest)
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("destination %s is not a directory", opts.Dest)
	}
	// a dry run must not write anything, so it only locks an existing mirror
	if _, serr := os.Stat(filepath.Join(opts.Dest, stateDirName)); !opts.DryRun || serr == nil {
		release, err := lockDest(opts.Dest, !opts.DryRun)
		if err != nil {
			return nil, explainFSError(err)
		}
		defer release()
	}
	if !opts.DryRun {
		removeStaleTemps(opts.Dest, opts.Logf)
	}

	man, err := loadManifest(opts.Dest)
	if err != nil {
		return nil, err
	}
	if man.AppleID != "" && opts.AppleID != "" && !strings.EqualFold(man.AppleID, opts.AppleID) {
		return nil, fmt.Errorf("destination %s holds a mirror of %s, refusing to sync %s into it", opts.Dest, man.AppleID, opts.AppleID)
	}
	if opts.AppleID != "" {
		man.AppleID = opts.AppleID
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := &engine{opts: opts, man: man, lastSave: time.Now(), cancel: cancel, trashAt: trashStampFor(time.Now())}

	all := map[string]*Stats{}
	for _, svc := range services {
		st := e.runService(ctx, svc)
		all[svc.Name] = st
		if e.fatalErr != nil || ctx.Err() != nil {
			break
		}
	}
	if !opts.DryRun {
		purgeTrash(opts.Dest, opts.TrashDays, time.Now(), opts.Logf)
		e.mu.Lock()
		serr := man.save(opts.Dest)
		e.mu.Unlock()
		if serr != nil {
			return all, fmt.Errorf("saving manifest: %w", explainFSError(serr))
		}
	}
	if e.fatalErr != nil {
		return all, e.fatalErr
	}
	return all, ctx.Err()
}

func (e *engine) runService(ctx context.Context, svc Service) *Stats {
	e.mu.Lock()
	entries := e.man.Services[svc.Name]
	if entries == nil {
		entries = map[string]Entry{}
		e.man.Services[svc.Name] = entries
	}
	s := &svcState{e: e, name: svc.Name, entries: entries, reserved: map[string]string{}, seen: map[string]bool{}, prior: len(entries), stats: &Stats{}}
	for k, en := range entries {
		s.reserved[foldKey(en.Path)] = k
	}
	e.mu.Unlock()

	jobs := make(chan job, e.opts.Workers*2)
	var wg sync.WaitGroup
	for i := 0; i < e.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				s.download(ctx, j)
			}
		}()
	}
	s.stats.Partial = svc.Partial // before the progress goroutine starts reading the stats
	stopProgress := s.reportProgress()
	listErr := svc.List(ctx, func(it Item) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if svc.UntilFound > 0 && s.groupEnded(it, svc.UntilFound) {
			return ErrStopListing
		}
		e.mu.Lock()
		skippedBefore := s.stats.Skipped
		e.mu.Unlock()
		if err := s.process(ctx, it, jobs); err != nil {
			return err
		}
		if svc.UntilFound > 0 {
			e.mu.Lock()
			upToDate := s.stats.Skipped > skippedBefore
			e.mu.Unlock()
			s.noteItem(it, upToDate)
		}
		return nil
	})
	close(jobs)
	wg.Wait()
	stopProgress()
	if errors.Is(listErr, ErrStopListing) {
		listErr = nil
		e.mu.Lock()
		s.stats.StoppedEarly = true
		if !svc.Partial {
			// a Lister that stops early without declaring itself partial must never delete
			s.stats.Partial = true
		}
		e.mu.Unlock()
	}

	if listErr != nil {
		s.stats.ListError = listErr
		e.opts.Logf("[%s] listing incomplete, nothing will be deleted this run: %v", svc.Name, listErr)
		return s.stats
	}
	if ctx.Err() != nil || e.fatalErr != nil {
		return s.stats
	}
	if s.stats.Partial {
		e.opts.Logf("[%s] partial listing, nothing is deleted this run", svc.Name)
		return s.stats
	}
	if !e.opts.NoDelete {
		s.mirrorDeletes()
	}
	return s.stats
}

// noteItem records whether an item was already up to date, per group.
func (s *svcState) noteItem(it Item, upToDate bool) {
	g := it.Group
	if g == "" {
		g = it.Key
	}
	if !s.groupSeen || g != s.group {
		s.group, s.groupSeen, s.groupUpToDate = g, true, upToDate
		return
	}
	s.groupUpToDate = s.groupUpToDate && upToDate
}

// groupEnded is called before an item is processed. When it starts a new
// group, the previous group is finished: if the last n groups were all up to
// date the listing can stop (--until-found).
func (s *svcState) groupEnded(next Item, n int) bool {
	g := next.Group
	if g == "" {
		g = next.Key
	}
	if !s.groupSeen || g == s.group {
		return false
	}
	if s.groupUpToDate {
		s.foundRun++
	} else {
		s.foundRun = 0
	}
	s.groupSeen = false // finished: the next noteItem opens a new group
	return s.foundRun >= n
}

// allocate picks a free local path for it; caller holds e.mu. A path is free
// when no other item reserved it and nothing the program does not own sits
// there on disk (a file of the right size may be adopted when the item has no
// manifest entry yet).
func (s *svcState) allocate(it Item, prev Entry, had bool) string {
	cand := it.Desired
	for n := 2; ; n++ {
		owner, taken := s.reserved[foldKey(cand)]
		if taken && owner == it.Key {
			break
		}
		if !taken && s.freeOnDisk(cand, it, prev, had) {
			break
		}
		cand = withSuffix(it.Desired, n)
	}
	s.reserved[foldKey(cand)] = it.Key
	return cand
}

func (s *svcState) freeOnDisk(rel string, it Item, prev Entry, had bool) bool {
	abs, err := safeJoin(s.e.opts.Dest, rel)
	if err != nil {
		return true // rejected later with a proper error
	}
	fi, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if !had {
		// adoption: an interrupted run wrote the file but did not save the manifest
		return fi.Mode().IsRegular() && fi.Size() == it.Size
	}
	if oldAbs, err := safeJoin(s.e.opts.Dest, prev.Path); err == nil {
		if ofi, err := os.Lstat(oldAbs); err == nil {
			// a case-only rename: the item's own file under another case
			return foldKey(prev.Path) == foldKey(rel) && os.SameFile(ofi, fi)
		}
	}
	return finishedBeforeKill(abs, it, prev)
}

// present reports whether en's content is at abs: a regular file of the
// recorded size, or a directory for an unpacked package.
func present(abs string, en Entry) bool {
	fi, err := os.Lstat(abs)
	if err != nil {
		return false
	}
	if en.Dir {
		return fi.IsDir()
	}
	return fi.Mode().IsRegular() && fi.Size() == en.Size
}

// finishedBeforeKill: the old path is gone and abs holds what a move (same
// version) or a re-download (new size) would have produced, i.e. a run was
// killed after doing the work but before saving the manifest.
func finishedBeforeKill(abs string, it Item, prev Entry) bool {
	if prev.Version == it.Version && present(abs, prev) {
		return true
	}
	size, ok := fileSize(abs)
	return ok && !prev.Dir && size == it.Size
}

// release frees rel if key owns it; caller holds e.mu.
func (s *svcState) release(key, rel string) bool {
	fk := foldKey(rel)
	if owner, ok := s.reserved[fk]; ok && owner != key {
		return false
	}
	delete(s.reserved, fk)
	return true
}

func (s *svcState) process(ctx context.Context, it Item, jobs chan<- job) error {
	e := s.e
	e.mu.Lock()
	if s.seen[it.Key] {
		e.mu.Unlock()
		return nil
	}
	s.seen[it.Key] = true
	s.stats.Listed++
	prev, had := s.entries[it.Key]
	// allocate returns the previous path when nothing changed, and lets an item
	// drop a collision suffix once the name it wants is free again
	rel := s.allocate(it, prev, had)
	e.mu.Unlock()

	abs, err := safeJoin(e.opts.Dest, rel)
	if err != nil {
		s.fail(it, err)
		return nil
	}
	size, exists := fileSize(abs)

	switch {
	case had && prev.Path == rel && prev.Version == it.Version && present(abs, prev):
		if prev.Dir && !e.opts.DryRun {
			if fixed, err := flattenPackage(abs); err != nil {
				e.opts.Logf("[%s] could not repair package %s: %v", s.name, rel, err)
			} else if fixed {
				e.opts.Logf("[%s] repaired nested package %s", s.name, rel)
			}
		}
		e.mu.Lock()
		s.stats.Skipped++
		e.mu.Unlock()
		return nil

	case had && prev.Path != rel && !fileExists(e.opts.Dest, prev.Path) && finishedBeforeKill(abs, it, prev):
		e.mu.Lock()
		if !e.opts.DryRun {
			en := Entry{Path: rel, Desired: it.Desired, Version: it.Version, Size: size, Dir: prev.Dir}
			if prev.Dir {
				en.Size = prev.Size
			}
			s.entries[it.Key] = en
			if foldKey(prev.Path) != foldKey(rel) {
				s.release(it.Key, prev.Path)
			}
			e.maybeSave()
		}
		s.stats.Adopted++
		e.mu.Unlock()
		return nil

	case had && prev.Path != rel && prev.Version == it.Version:
		if oldAbs, err := safeJoin(e.opts.Dest, prev.Path); err == nil {
			if present(oldAbs, prev) {
				if e.opts.DryRun {
					e.opts.Logf("[%s] would move %s -> %s", s.name, prev.Path, rel)
					return nil
				}
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err == nil {
					if err := moveFile(oldAbs, abs); err == nil {
						e.opts.Logf("[%s] moved %s -> %s", s.name, prev.Path, rel)
						e.mu.Lock()
						if foldKey(prev.Path) != foldKey(rel) { // same key: that reservation is ours now
							s.release(it.Key, prev.Path)
						}
						s.entries[it.Key] = Entry{Path: rel, Desired: it.Desired, Version: it.Version, Size: prev.Size, Dir: prev.Dir}
						s.stats.Moved++
						e.maybeSave()
						e.mu.Unlock()
						pruneEmptyDirs(filepath.Dir(oldAbs), e.opts.Dest)
						return nil
					}
				}
			}
		}

	case !had && exists && size == it.Size:
		// a previous run was interrupted after writing the file but before saving the manifest
		e.mu.Lock()
		if !e.opts.DryRun {
			s.entries[it.Key] = Entry{Path: rel, Desired: it.Desired, Version: it.Version, Size: it.Size}
			e.maybeSave()
		}
		s.stats.Adopted++
		e.mu.Unlock()
		return nil
	}

	select {
	case jobs <- job{it: it, rel: rel, prev: prev, hadPrev: had}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maybeSave writes the manifest if the last save is older than 30 s, so a
// killed run loses little bookkeeping; caller holds e.mu.
func (e *engine) maybeSave() {
	if e.opts.DryRun || time.Since(e.lastSave) < 30*time.Second {
		return
	}
	if err := e.man.save(e.opts.Dest); err == nil {
		e.lastSave = time.Now()
	}
}

func fileExists(dest, rel string) bool {
	abs, err := safeJoin(dest, rel)
	if err != nil {
		return false
	}
	_, err = os.Lstat(abs)
	return err == nil
}

// ProgressInterval is how often a running service prints a progress line.
var ProgressInterval = 30 * time.Second

// reportProgress prints a line every ProgressInterval so long runs that mostly
// skip unchanged files do not look stuck. The returned func stops it.
func (s *svcState) reportProgress() func() {
	done := make(chan struct{})
	interval := ProgressInterval // read here, not in the goroutine: tests change it between runs
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.e.mu.Lock()
				st := *s.stats
				s.e.mu.Unlock()
				s.e.opts.Logf("[%s] progress: listed %d, downloaded %d (%s), unchanged %d, moved %d, adopted %d, failed %d",
					s.name, st.Listed, st.Downloaded, humanBytes(st.Bytes), st.Skipped, st.Moved, st.Adopted, st.Failed)
			}
		}
	}()
	return func() { close(done) }
}

func (s *svcState) fail(it Item, err error) {
	s.e.mu.Lock()
	s.stats.Failed++
	s.e.mu.Unlock()
	s.e.opts.Logf("[%s] FAILED %s: %v", s.name, it.Desired, err)
}

func (s *svcState) download(ctx context.Context, j job) {
	e := s.e
	if ctx.Err() != nil {
		return
	}
	if e.opts.DryRun {
		e.opts.Logf("[%s] would download %s (%s)", s.name, j.rel, humanBytes(j.it.Size))
		return
	}
	abs, err := safeJoin(e.opts.Dest, j.rel)
	if err != nil {
		s.fail(j.it, err)
		return
	}
	var written int64
	var isDir bool
	for attempt := 1; ; attempt++ {
		written, isDir, err = fetchTo(ctx, j.it, abs)
		if err == nil || ctx.Err() != nil || attempt > len(icloud.RetryDelays) {
			break
		}
		if !icloud.IsTransient(err) && !errors.Is(err, errShortDownload) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(icloud.RetryDelays[attempt-1]):
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		err = explainFSError(err)
		s.fail(j.it, err)
		fatal := error(nil)
		switch {
		case isFatalFSError(err):
			fatal = err
		case icloud.IsAuthError(err):
			fatal = fmt.Errorf("iCloud session expired during the sync (next run signs in again): %w", err)
		}
		if fatal != nil {
			e.mu.Lock()
			if e.fatalErr == nil {
				e.fatalErr = fatal
			}
			e.mu.Unlock()
			e.cancel()
		}
		return
	}
	e.opts.Logf("[%s] downloaded %s (%s)", s.name, j.rel, humanBytes(written))

	e.mu.Lock()
	s.entries[j.it.Key] = Entry{Path: j.rel, Desired: j.it.Desired, Version: j.it.Version, Size: written, Dir: isDir}
	s.stats.Downloaded++
	s.stats.Bytes += written
	e.maybeSave()
	e.mu.Unlock()

	// the item moved and changed: drop its old file. The old path stays
	// reserved until the file is gone, so nobody else can take it meanwhile.
	if j.hadPrev && j.prev.Path != j.rel {
		oldAbs, err := safeJoin(e.opts.Dest, j.prev.Path)
		if err != nil {
			return
		}
		if foldKey(j.prev.Path) == foldKey(j.rel) {
			// case-only rename: on a case-insensitive disk both names are the new file
			if a, err1 := os.Stat(oldAbs); err1 == nil {
				if b, err2 := os.Stat(abs); err2 == nil && os.SameFile(a, b) {
					_ = moveFile(abs, abs) // make the on-disk name take the new case
					return
				}
			}
			_ = removeEntryPath(oldAbs, j.prev)
			return
		}
		e.mu.Lock()
		owner, ok := s.reserved[foldKey(j.prev.Path)]
		mine := !ok || owner == j.it.Key
		e.mu.Unlock()
		if mine {
			_ = removeEntryPath(oldAbs, j.prev)
			pruneEmptyDirs(filepath.Dir(oldAbs), e.opts.Dest)
			e.mu.Lock()
			s.release(j.it.Key, j.prev.Path)
			e.mu.Unlock()
		}
	}
}

// removeStaleTemps deletes temp files left by a killed run (a second Ctrl+C,
// an unplugged drive). Called with the destination locked.
func removeStaleTemps(dest string, logf func(string, ...any)) {
	n := 0
	for _, root := range []string{DriveRoot, PhotosRoot} {
		n += removeTempsUnder(filepath.Join(dest, root))
	}
	if n > 0 {
		logf("removed %d unfinished temporary file(s) from an earlier run", n)
	}
}

func removeTempsUnder(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == stateDirName {
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".icloudsync-") && strings.HasSuffix(d.Name(), ".part.d") {
				if os.RemoveAll(p) == nil {
					n++
				}
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".icloudsync-") && strings.HasSuffix(d.Name(), ".part") {
			if os.Remove(p) == nil {
				n++
			}
		}
		return nil
	})
	return n
}

// moveFile renames old to new without replacing an unrelated file at new. A
// case-only rename goes through a temporary name so that case-insensitive
// file systems really change the name.
func moveFile(oldAbs, newAbs string) error {
	if strings.EqualFold(oldAbs, newAbs) {
		// only when both names are really the same file (case-insensitive
		// disk); on a case-sensitive disk the other name may be unrelated
		if a, err := os.Lstat(oldAbs); err == nil {
			if b, err := os.Lstat(newAbs); err == nil && !os.SameFile(a, b) {
				return fs.ErrExist
			}
		}
		tmp := filepath.Join(filepath.Dir(newAbs), fmt.Sprintf(".icloudsync-move-%d.part", time.Now().UnixNano()))
		if err := os.Rename(oldAbs, tmp); err != nil {
			return err
		}
		return os.Rename(tmp, newAbs)
	}
	if _, err := os.Lstat(newAbs); err == nil {
		return fs.ErrExist
	}
	return os.Rename(oldAbs, newAbs)
}

var errShortDownload = errors.New("short download")

// PackageBody marks content that is a zip archive of a package (an .app,
// .textbundle, ... directory): it is unpacked into a directory at the item's
// path, and its size is not compared with the listed size.
type PackageBody struct{ io.ReadCloser }

// fetchTo downloads it into abs atomically and returns the bytes written and
// whether abs is now an unpacked package directory.
func fetchTo(ctx context.Context, it Item, abs string) (int64, bool, error) {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, false, err
	}
	rc, length, err := it.Open(ctx)
	if err != nil {
		return 0, false, err
	}
	defer rc.Close()
	_, isPackage := rc.(PackageBody)
	tmp, err := os.CreateTemp(dir, ".icloudsync-*.part")
	if err != nil {
		return 0, false, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, rc)
	if err != nil {
		return 0, false, err
	}
	want := length
	if want < 0 && !isPackage {
		want = it.Size
	}
	if want > 0 && n != want {
		return 0, false, fmt.Errorf("%w: got %d of %d bytes", errShortDownload, n, want)
	}
	if err := tmp.Sync(); err != nil {
		return 0, false, err
	}
	if err := tmp.Close(); err != nil {
		return 0, false, err
	}
	if isPackage && isZipFile(tmp.Name()) {
		if err := installPackage(tmp.Name(), abs); err != nil {
			return 0, false, err
		}
		os.Remove(tmp.Name())
		ok = true
		if !it.ModTime.IsZero() {
			_ = os.Chtimes(abs, it.ModTime, it.ModTime)
		}
		return n, true, nil
	}
	if fi, err := os.Lstat(abs); err == nil && fi.IsDir() {
		// was an unpacked package before; only our own paths get here
		if err := os.RemoveAll(abs); err != nil {
			return 0, false, err
		}
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return 0, false, err
	}
	ok = true
	if !it.ModTime.IsZero() {
		_ = os.Chtimes(abs, it.ModTime, it.ModTime)
	}
	return n, false, nil
}

// removeEntryPath deletes an entry's file, or its directory for a package.
func removeEntryPath(abs string, en Entry) error {
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if en.Dir && fi.IsDir() {
		return os.RemoveAll(abs)
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory, expected a file", abs)
	}
	return os.Remove(abs)
}

func (s *svcState) mirrorDeletes() {
	e := s.e
	e.mu.Lock()
	var gone []string
	for k := range s.entries {
		if !s.seen[k] {
			gone = append(gone, k)
		}
	}
	total := s.prior
	seen := len(s.seen)
	e.mu.Unlock()
	if len(gone) == 0 {
		return
	}
	reason := ""
	switch {
	case seen == 0:
		// never overridden: an empty listing is far more often an iCloud
		// glitch than a really emptied library
		reason = fmt.Sprintf("iCloud returned no files but %d are on the drive", len(gone))
	case !e.opts.ForceDelete && len(gone) > 50 && len(gone)*4 > total:
		reason = fmt.Sprintf("%d of %d files (more than 25%%) would be deleted; re-run with --force-delete if this is intended", len(gone), total)
	}
	if reason != "" {
		s.stats.DeletionsBlocked = reason
		e.opts.Logf("[%s] SAFETY: %s; not deleting.", s.name, reason)
		return
	}
	for _, k := range gone {
		e.mu.Lock()
		en := s.entries[k]
		e.mu.Unlock()
		abs, err := safeJoin(e.opts.Dest, en.Path)
		if err != nil {
			continue
		}
		if e.opts.DryRun {
			if e.opts.Trash {
				e.opts.Logf("[%s] would move %s to the trash", s.name, en.Path)
			} else {
				e.opts.Logf("[%s] would delete %s", s.name, en.Path)
			}
			continue
		}
		if e.opts.Trash {
			if _, err := os.Lstat(abs); err == nil {
				if err := moveToTrash(e.opts.Dest, e.trashAt, en.Path, abs); err != nil {
					// keep the manifest entry: the file is still here and will be retried
					e.opts.Logf("[%s] could not move %s to the trash: %v", s.name, en.Path, err)
					continue
				}
			}
			e.opts.Logf("[%s] moved %s to the trash (%s)", s.name, en.Path, filepath.Join(stateDirName, trashDirName, e.trashAt))
		} else {
			if err := removeEntryPath(abs, en); err != nil && !errors.Is(err, fs.ErrNotExist) {
				e.opts.Logf("[%s] could not delete %s: %v", s.name, en.Path, err)
				continue
			}
			e.opts.Logf("[%s] deleted %s", s.name, en.Path)
		}
		pruneEmptyDirs(filepath.Dir(abs), e.opts.Dest)
		e.mu.Lock()
		delete(s.entries, k)
		s.release(k, en.Path)
		s.stats.Deleted++
		e.maybeSave()
		e.mu.Unlock()
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func HumanBytes(n int64) string { return humanBytes(n) }
