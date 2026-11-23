package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/serebro/icloudsync/internal/icloud"
)

func runPhotos(t *testing.T, f *fakeICloud, dest string, po PhotoOptions) *Stats {
	t.Helper()
	svc := PhotosServicesWith(f.client(t), t.Logf, po)[0]
	return runServices(t, dest, svc)["photos"]
}

func manifestPaths(t *testing.T, dest, svc string) map[string]string {
	t.Helper()
	man, err := loadManifest(dest)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, e := range man.Services[svc] {
		out[k] = e.Path
	}
	return out
}

func TestPlainListingRequestIsUnchanged(t *testing.T) {
	f := newFakeICloud(t)
	runPhotos(t, f, t.TempDir(), PhotoOptions{})
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.lastKeys {
		if strings.HasPrefix(k, "resJPEG") || strings.HasPrefix(k, "resOriginalAlt") || k == "captionEnc" || k == "locationEnc" || k == "keywordsEnc" {
			t.Errorf("a plain run asks Apple for %q", k)
		}
	}
	if len(f.lastKeys) != 13 || f.lastDirection != "ASCENDING" {
		t.Fatalf("keys %d direction %q", len(f.lastKeys), f.lastDirection)
	}
}

func TestSizeOriginalAndAdjusted(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	st := runPhotos(t, f, dest, PhotoOptions{Sizes: []icloud.PhotoSize{icloud.SizeOriginal, icloud.SizeAdjusted}})
	if st.ListError != nil {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	for p, body := range map[string]string{
		"Photos/Primary/2024/05/17/IMG_1.HEIC":         "/content/photos/m1",
		"Photos/Primary/2024/05/17/IMG_1-adjusted.JPG": "/content/photos/a1edit", // extension follows the rendition
		"Photos/Primary/2024/05/17/IMG_1_HEVC.MOV":     "/content/photos/m1live",
		"Photos/Primary/unknown-date/a2-012345678.JPG": "/content/photos/m2", // no edit: no duplicate of the original
		"Photos/Primary/2024/05/17/clip.mov":           "/content/photos/m3",
	} {
		if got[p] != body {
			t.Errorf("%s = %q", p, got[p])
		}
	}
	if len(got) != 5 {
		t.Fatalf("tree %v", got)
	}
	keys := manifestPaths(t, dest, "photos")
	if keys["a1"] != "Photos/Primary/2024/05/17/IMG_1.HEIC" || keys["a1#adjusted"] != "Photos/Primary/2024/05/17/IMG_1-adjusted.JPG" {
		t.Fatalf("keys %v", keys)
	}
}

func TestSwitchingToAdjustedReplacesInPlace(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	runPhotos(t, f, dest, PhotoOptions{})
	st := runPhotos(t, f, dest, PhotoOptions{Sizes: []icloud.PhotoSize{icloud.SizeAdjusted}})
	if st.ListError != nil || st.Deleted != 0 || st.Failed != 0 {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	// the edited photo replaces its original under the same key; the unedited one is untouched
	if got["Photos/Primary/2024/05/17/IMG_1.JPG"] != "/content/photos/a1edit" {
		t.Fatalf("tree %v", got)
	}
	if _, ok := got["Photos/Primary/2024/05/17/IMG_1.HEIC"]; ok {
		t.Fatal("the original was left behind next to its replacement")
	}
	if got["Photos/Primary/unknown-date/a2-012345678.JPG"] != "/content/photos/m2" {
		t.Fatalf("a photo without an edit must fall back to its original: %v", got)
	}
}

func TestThumbSuffixAndForceSize(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	runPhotos(t, f, dest, PhotoOptions{Sizes: []icloud.PhotoSize{icloud.SizeThumb}, ForceSize: true})
	got := tree(t, dest)
	if got["Photos/Primary/2024/05/17/IMG_1-thumb.JPG"] != "/content/photos/m1thumb" {
		t.Fatalf("tree %v", got)
	}
	// without a thumb, with --force-size, nothing is downloaded for that photo;
	// videos are always the original
	for p := range got {
		if strings.Contains(p, "a2-") {
			t.Fatalf("--force-size downloaded a photo without the rendition: %s", p)
		}
	}
	if got["Photos/Primary/2024/05/17/clip.mov"] == "" {
		t.Fatalf("videos ignore --size: %v", got)
	}
}

func TestXMPSidecar(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	po := PhotoOptions{XMP: true}
	st := runPhotos(t, f, dest, po)
	if st.ListError != nil || st.Failed != 0 {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	x := got["Photos/Primary/2024/05/17/IMG_1.HEIC.xmp"]
	for _, want := range []string{
		"<x:xmpmeta", "Sunset", "<rdf:li>sunset</rdf:li>", "beach &lt;3", "<xmp:Rating>5</xmp:Rating>",
		"<exif:GPSLatitude>52,30.000000N</exif:GPSLatitude>", "<exif:GPSLongitude>13,15.000000W</exif:GPSLongitude>",
		"<exif:GPSAltitude>3400/100</exif:GPSAltitude>",
		"2024-05-17T",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("sidecar lacks %q:\n%s", want, x)
		}
	}
	if strings.Contains(x, "+01:00") == false && strings.Contains(x, "+0100") == false {
		t.Errorf("the photo's own time zone offset (3600 s) is not used:\n%s", x)
	}
	// photos without metadata get no sidecar
	n := 0
	for p := range got {
		if strings.HasSuffix(p, ".xmp") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d sidecars, want 1: %v", n, got)
	}

	// unchanged metadata must not re-download or rewrite anything
	f.srv.CloseClientConnections()
	st = runPhotos(t, f, dest, po)
	if st.Downloaded != 0 || st.Skipped != 5 {
		t.Fatalf("second run re-wrote files: %+v", st)
	}

	// without the flag the sidecars go away (mirror semantics), the photos stay
	st = runPhotos(t, f, dest, PhotoOptions{})
	if st.Deleted != 1 {
		t.Fatalf("%+v", st)
	}
	got = tree(t, dest)
	if _, ok := got["Photos/Primary/2024/05/17/IMG_1.HEIC.xmp"]; ok || len(got) != 4 {
		t.Fatalf("tree %v", got)
	}
}

func TestBuildXMPIsDeterministicAndEscaped(t *testing.T) {
	m := &icloud.PhotoMeta{Caption: `a & "b"`, Favorite: true, Screenshot: true}
	a, b := buildXMP(m), buildXMP(m)
	if string(a) != string(b) {
		t.Fatal("output differs between calls")
	}
	if !strings.Contains(string(a), "a &amp; &#34;b&#34;") || !strings.Contains(string(a), "screenCapture") {
		t.Fatalf("%s", a)
	}
	for in, want := range map[float64]string{52.5: "52,30.000000N", 0.9999999999: "1,0.000000N"} {
		if got := gpsCoord(in, "N", "S"); got != want {
			t.Errorf("gpsCoord(%v) = %s, want %s", in, got, want)
		}
	}
	if (&icloud.PhotoMeta{}).Empty() != true || (*icloud.PhotoMeta)(nil).Empty() != true {
		t.Fatal("empty metadata must not produce a sidecar")
	}
}

func runAlbums(t *testing.T, f *fakeICloud, dest string, po PhotoOptions) *Stats {
	t.Helper()
	return runServices(t, dest, PersonalAlbumsService(f.client(t), t.Logf, po))["photos-myalbums"]
}

func TestPersonalAlbums(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	st := runAlbums(t, f, dest, PhotoOptions{AllAlbums: true})
	if st.ListError != nil || st.Downloaded != 3 {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	for p, body := range map[string]string{
		"Photos/Albums/Family/2024/05/17/IMG_1.HEIC":   "/content/photos/m1",
		"Photos/Albums/Family/2024/05/17/clip.mov":     "/content/photos/m3",
		"Photos/Albums/Trips/Rome/2024/05/17/clip.mov": "/content/photos/m3", // an album inside a folder; the deleted one is skipped
	} {
		if got[p] != body {
			t.Errorf("%s = %q (tree %v)", p, got[p], got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("tree %v", got)
	}
	if k := manifestPaths(t, dest, "photos-myalbums"); k["A1/a1"] == "" || k["A2/a3"] == "" {
		t.Fatalf("keys carry the album id: %v", k)
	}
}

func TestPersonalAlbumSelection(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	// by the album's own name
	st := runAlbums(t, f, dest, PhotoOptions{Albums: []string{"Rome"}})
	if st.ListError != nil || st.Downloaded != 1 {
		t.Fatalf("%+v", st)
	}
	if _, ok := tree(t, dest)["Photos/Albums/Trips/Rome/2024/05/17/clip.mov"]; !ok {
		t.Fatalf("tree %v", tree(t, dest))
	}
	// an unknown album is an error that blocks deletions, and lists what exists
	st = runAlbums(t, f, dest, PhotoOptions{Albums: []string{"Nope"}})
	if st.ListError == nil || !strings.Contains(st.ListError.Error(), "Family") || st.Deleted != 0 {
		t.Fatalf("%+v", st)
	}
	if len(tree(t, dest)) != 1 {
		t.Fatal("files were deleted after a failed listing")
	}
}

func TestPersonalAlbumCountMismatchBlocksDeletes(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	runAlbums(t, f, dest, PhotoOptions{AllAlbums: true})
	f.mu.Lock()
	f.myAlbumCounts["A1"] = 9
	f.mu.Unlock()
	st := runAlbums(t, f, dest, PhotoOptions{AllAlbums: true})
	if st.ListError == nil || !strings.Contains(st.ListError.Error(), "Family") || st.Deleted != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestRecentStopsEarlyAndDeletesNothing(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	full := runPhotos(t, f, dest, PhotoOptions{})
	if full.Downloaded != 4 {
		t.Fatalf("%+v", full)
	}
	st := runPhotos(t, f, dest, PhotoOptions{Recent: 1})
	if st.ListError != nil || !st.Partial || !st.StoppedEarly || st.Listed != 2 /* photo + Live video */ || st.Deleted != 0 {
		t.Fatalf("%+v", st)
	}
	f.mu.Lock()
	dir := f.lastDirection
	f.mu.Unlock()
	if dir != RecentDirection {
		t.Fatalf("direction %q", dir)
	}
	if len(tree(t, dest)) != 4 {
		t.Fatal("a partial run deleted files")
	}
	// a fresh mirror holds only what was listed
	dest2 := t.TempDir()
	runPhotos(t, f, dest2, PhotoOptions{Recent: 2})
	if n := len(tree(t, dest2)); n != 3 { // IMG_1 + its Live video + a2
		t.Fatalf("tree %v", tree(t, dest2))
	}
}

func TestRecentIsPerAlbum(t *testing.T) {
	f := newFakeICloud(t)
	st := runAlbums(t, f, t.TempDir(), PhotoOptions{AllAlbums: true, Recent: 1})
	// Family holds two photos, Rome one: one from each album, not one in total
	if st.ListError != nil || !st.Partial || st.Listed != 2 || st.Deleted != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestUntilFoundCountsConsecutiveUpToDatePhotos(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	runPhotos(t, f, dest, PhotoOptions{})

	st := runPhotos(t, f, dest, PhotoOptions{UntilFound: 1})
	if !st.StoppedEarly || st.Listed != 2 || st.Skipped != 2 || st.Deleted != 0 {
		t.Fatalf("until-found 1: %+v", st)
	}
	st = runPhotos(t, f, dest, PhotoOptions{UntilFound: 2})
	if !st.StoppedEarly || st.Listed != 3 {
		t.Fatalf("until-found 2: %+v", st)
	}
	// a photo that went missing from the disk resets the run, and is repaired
	if err := os.Remove(filepath.Join(dest, "Photos/Primary/unknown-date/a2-012345678.JPG")); err != nil {
		t.Fatal(err)
	}
	st = runPhotos(t, f, dest, PhotoOptions{UntilFound: 2})
	if st.StoppedEarly || st.Downloaded != 1 {
		t.Fatalf("until-found after a gap: %+v", st)
	}
	// without either flag nothing is partial
	if st := runPhotos(t, f, dest, PhotoOptions{}); st.Partial || st.StoppedEarly {
		t.Fatalf("%+v", st)
	}
}

func TestPartialServiceNeverDeletes(t *testing.T) {
	dest := t.TempDir()
	r := &fakeRemote{files: []remoteFile{
		{"1", "Drive/a.txt", "aaa", "v1"},
		{"2", "Drive/b.txt", "bb", "v1"},
	}}
	runOnce(t, dest, r, nil)
	r.files = r.files[:1]
	svc := r.service("drive")
	svc.Partial = true
	st, err := Run(t.Context(), Options{Dest: dest, AppleID: "me@example.com", Workers: 1, Logf: t.Logf}, []Service{svc})
	if err != nil {
		t.Fatal(err)
	}
	if st["drive"].Deleted != 0 || !st["drive"].Partial || len(tree(t, dest)) != 2 {
		t.Fatalf("%+v %v", st["drive"], tree(t, dest))
	}
}

func TestParsePhotoSizes(t *testing.T) {
	got, err := icloud.ParsePhotoSizes("Original, adjusted,original")
	if err != nil || len(got) != 2 || got[0] != icloud.SizeOriginal || got[1] != icloud.SizeAdjusted {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "huge", "original,,x"} {
		if _, err := icloud.ParsePhotoSizes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
