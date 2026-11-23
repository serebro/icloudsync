package syncer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/serebro/icloudsync/internal/icloud"
)

// fakeICloud serves the subset of Drive, Photos (CloudKit) and Shared Streams
// that the services use. Content of every downloadable file is its URL path.
type fakeICloud struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	photoCount  int    // what HyperionIndexCountLookup reports
	indexState  string // CheckIndexingState
	albumCounts map[string]int
	brokenAlbum string // webgetassets for this album guid fails permanently

	lastKeys      []string       // desiredKeys of the last photo listing request
	lastDirection string         // direction filter of the last photo listing request
	myAlbumCounts map[string]int // personal album id -> count reported by the index
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func newFakeICloud(t *testing.T) *fakeICloud {
	f := &fakeICloud{t: t, photoCount: 3, indexState: "FINISHED", albumCounts: map[string]int{"G1": 2, "G2": 1},
		myAlbumCounts: map[string]int{"A1": 2, "A2": 1}}
	mux := http.NewServeMux()

	// ---- Drive ----
	mux.HandleFunc("POST /retrieveItemDetailsInFolders", func(w http.ResponseWriter, r *http.Request) {
		var req []struct{ Drivewsid string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		var out []string
		for _, q := range req {
			switch q.Drivewsid {
			case icloud.DriveRootID:
				out = append(out, `{"drivewsid":"`+icloud.DriveRootID+`","items":[
					{"drivewsid":"FILE::com.apple.CloudDocs::A","docwsid":"A","zone":"com.apple.CloudDocs","name":"notes","extension":"txt","type":"FILE","size":10,"etag":"e1","dateModified":"2024-01-02T03:04:05Z"},
					{"drivewsid":"FOLDER::com.apple.CloudDocs::D","name":"Docs: 2024","type":"FOLDER"}]}`)
			case "FOLDER::com.apple.CloudDocs::D":
				out = append(out, `{"drivewsid":"FOLDER::com.apple.CloudDocs::D","items":[
					{"drivewsid":"FILE::com.apple.CloudDocs::B","docwsid":"B","zone":"com.apple.CloudDocs","name":"report","extension":"pdf","type":"FILE","size":10,"etag":"e2","dateModified":"2024-01-02T03:04:05Z"}]}`)
			default:
				t.Errorf("unexpected folder %q", q.Drivewsid)
			}
		}
		fmt.Fprint(w, "["+strings.Join(out, ",")+"]")
	})
	mux.HandleFunc("GET /ws/com.apple.CloudDocs/download/by_id", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data_token":{"url":"%s/content/drive/%s"}}`, f.srv.URL, r.URL.Query().Get("document_id"))
	})

	// ---- Photos (CloudKit) ----
	const ck = "/database/1/com.apple.photos.cloud/production/private"
	mux.HandleFunc("POST "+ck+"/zones/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"zones":[{"zoneID":{"zoneName":"PrimarySync"}}]}`)
	})
	mux.HandleFunc("POST "+ck+"/records/query", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Query struct {
				RecordType string
				FilterBy   []struct {
					FieldName  string
					FieldValue struct{ Value any }
				}
			}
			DesiredKeys []string
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		filter := func(name string) any {
			for _, fb := range q.Query.FilterBy {
				if fb.FieldName == name {
					return fb.FieldValue.Value
				}
			}
			return nil
		}
		switch q.Query.RecordType {
		case "CheckIndexingState":
			f.mu.Lock()
			st := f.indexState
			f.mu.Unlock()
			fmt.Fprintf(w, `{"records":[{"recordType":"CheckIndexingState","fields":{"state":{"value":%q}}}]}`, st)
			return
		case "CPLAlbumByPositionLive":
			album := func(id, name string, typ int) string {
				return fmt.Sprintf(`{"recordName":%q,"recordType":"CPLAlbumByPositionLive","fields":{"albumNameEnc":{"value":%q},"albumType":{"value":%d}}}`, id, b64(name), typ)
			}
			root := `{"recordName":"----Root-Folder----","recordType":"CPLAlbumByPositionLive","fields":{"albumNameEnc":{"value":"` + b64("root") + `"},"albumType":{"value":3}}}`
			deleted := `{"recordName":"AX","recordType":"CPLAlbumByPositionLive","fields":{"albumNameEnc":{"value":"` + b64("Gone") + `"},"albumType":{"value":0},"isDeleted":{"value":1}}}`
			if filter("parentId") == "F1" {
				fmt.Fprint(w, `{"records":[`+album("A2", "Rome", 0)+`]}`)
			} else {
				fmt.Fprint(w, `{"records":[`+root+","+album("A1", "Family", 0)+","+album("F1", "Trips", 3)+","+deleted+`]}`)
			}
			return
		}
		f.mu.Lock()
		f.lastKeys = q.DesiredKeys
		f.lastDirection, _ = filter("direction").(string)
		f.mu.Unlock()
		if int(filter("startRank").(float64)) != 0 {
			fmt.Fprint(w, `{"records":[]}`)
			return
		}
		day := time.Date(2024, 5, 17, 12, 0, 0, 0, time.Local).UnixMilli()
		res := func(id string) string {
			return fmt.Sprintf(`{"value":{"size":%d,"downloadURL":"%s/content/photos/%s","fileChecksum":"C%s"}}`, len("/content/photos/"+id), f.srv.URL, id, id)
		}
		var recs []string
		if q.Query.RecordType == "CPLContainerRelationLiveByAssetDate" {
			// personal albums: A1 holds the first two photos, A2 the third
			switch filter("parentId") {
			case "A1":
				recs = append(recs,
					fmt.Sprintf(`{"recordName":"a1","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m1"}},"assetDate":{"value":%d}}}`, day),
					fmt.Sprintf(`{"recordName":"m1","recordType":"CPLMaster","fields":{"filenameEnc":{"type":"ENCRYPTED_BYTES","value":%q},"itemType":{"value":"public.heic"},"resOriginalRes":%s,"resOriginalFileType":{"value":"public.heic"}}}`, b64("IMG_1.HEIC"), res("m1")),
					fmt.Sprintf(`{"recordName":"a3","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m3"}},"assetDate":{"value":%d}}}`, day),
					fmt.Sprintf(`{"recordName":"m3","recordType":"CPLMaster","fields":{"filenameEnc":{"type":"ENCRYPTED_BYTES","value":%q},"itemType":{"value":"com.apple.quicktime-movie"},"resOriginalRes":%s}}`, b64("clip.mov"), res("m3")))
			case "A2":
				recs = append(recs,
					fmt.Sprintf(`{"recordName":"a3","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m3"}},"assetDate":{"value":%d}}}`, day),
					fmt.Sprintf(`{"recordName":"m3","recordType":"CPLMaster","fields":{"filenameEnc":{"type":"ENCRYPTED_BYTES","value":%q},"itemType":{"value":"com.apple.quicktime-movie"},"resOriginalRes":%s}}`, b64("clip.mov"), res("m3")))
			}
			fmt.Fprint(w, `{"records":[`+strings.Join(recs, ",")+`]}`)
			return
		}
		loc, _ := plist.Marshal(map[string]any{"lat": 52.5, "lon": -13.25, "alt": 34.0}, plist.BinaryFormat)
		kw, _ := plist.Marshal([]string{"sunset", "beach <3"}, plist.BinaryFormat)
		recs = []string{
			// an edited photo: the asset carries the edit, the master the other renditions
			fmt.Sprintf(`{"recordName":"a1","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m1"}},"assetDate":{"value":%d},"resJPEGFullRes":%s,"resJPEGFullFileType":{"value":"public.jpeg"},"isFavorite":{"value":1},"captionEnc":{"type":"ENCRYPTED_BYTES","value":%q},"keywordsEnc":{"type":"ENCRYPTED_BYTES","value":%q},"locationEnc":{"type":"ENCRYPTED_BYTES","value":%q},"timeZoneOffset":{"value":3600}}}`,
				day, res("a1edit"), b64("Sunset"), base64.StdEncoding.EncodeToString(kw), base64.StdEncoding.EncodeToString(loc)),
			fmt.Sprintf(`{"recordName":"m1","recordType":"CPLMaster","fields":{"filenameEnc":{"type":"ENCRYPTED_BYTES","value":%q},"itemType":{"value":"public.heic"},"resOriginalRes":%s,"resOriginalFileType":{"value":"public.heic"},"resOriginalVidComplRes":%s,"resOriginalVidComplFileType":{"value":"com.apple.quicktime-movie"},"resJPEGThumbRes":%s,"resJPEGThumbFileType":{"value":"public.jpeg"}}}`, b64("IMG_1.HEIC"), res("m1"), res("m1live"), res("m1thumb")),
			// no date, no filename, no edit
			`{"recordName":"a2-0123456789abcdef","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m2"}}}}`,
			fmt.Sprintf(`{"recordName":"m2","recordType":"CPLMaster","fields":{"itemType":{"value":"public.jpeg"},"resOriginalRes":%s}}`, res("m2")),
			fmt.Sprintf(`{"recordName":"a3","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m3"}},"assetDate":{"value":%d}}}`, day),
			fmt.Sprintf(`{"recordName":"m3","recordType":"CPLMaster","fields":{"filenameEnc":{"type":"ENCRYPTED_BYTES","value":%q},"itemType":{"value":"com.apple.quicktime-movie"},"resOriginalRes":%s}}`, b64("clip.mov"), res("m3")),
		}
		fmt.Fprint(w, `{"records":[`+strings.Join(recs, ",")+`]}`)
	})
	mux.HandleFunc("POST "+ck+"/internal/records/query/batch", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Batch []struct {
				Query struct {
					FilterBy struct {
						FieldValue struct{ Value []string }
					}
				}
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		f.mu.Lock()
		n := f.photoCount
		if len(q.Batch) > 0 && len(q.Batch[0].Query.FilterBy.FieldValue.Value) > 0 {
			if id, ok := strings.CutPrefix(q.Batch[0].Query.FilterBy.FieldValue.Value[0], "CPLContainerRelationNotDeletedByAssetDate:"); ok {
				n = f.myAlbumCounts[id]
			}
		}
		f.mu.Unlock()
		fmt.Fprintf(w, `{"batch":[{"records":[{"recordType":"HyperionIndexCountLookup","fields":{"itemCount":{"value":%d}}}]}]}`, n)
	})

	// ---- Shared Streams ----
	mux.HandleFunc("POST /42/sharedstreams/webgetalbumslist", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"albums":[
			{"albumguid":"G1","albumlocation":"%[1]s/G1/sharedstreams/","albumctag":"c","sharingtype":"owned","attributes":{"name":"Trip"}},
			{"albumguid":"G2","albumlocation":"%[1]s/G2/sharedstreams/","albumctag":"c","sharingtype":"subscribed","attributes":{"name":"trip"}},
			{"albumguid":"G3","albumlocation":"%[1]s/G3/sharedstreams/","albumctag":"c","sharingtype":"owned","attributes":{"name":"../.."}}]}`, f.srv.URL)
	})
	mux.HandleFunc("POST /{guid}/sharedstreams/webgetassets", func(w http.ResponseWriter, r *http.Request) {
		guid := r.PathValue("guid")
		f.mu.Lock()
		broken := f.brokenAlbum == guid
		f.mu.Unlock()
		if broken {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errorCode":"BAD","errorMessage":"nope"}`)
			return
		}
		var q map[string]string
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q["offset"] != "0" {
			fmt.Fprint(w, `{"records":[]}`)
			return
		}
		n := map[string]int{"G1": 2, "G2": 1, "G3": 1}[guid]
		var recs []string
		for i := 0; i < n; i++ {
			m, a := fmt.Sprintf("%s-m%d", guid, i), fmt.Sprintf("%s-a%d", guid, i)
			p := "/content/albums/" + m
			recs = append(recs,
				fmt.Sprintf(`{"recordName":%q,"recordType":"CPLMaster","fields":{"filenameEnc":{"type":"BYTES","value":%q},"originalCreationDate":{"value":%d},"resOriginalFileSize":{"value":%d},"resOriginalFileType":{"value":"public.jpeg"},"resOriginalRes":{"value":{"downloadURL":"%s%s","fileChecksum":"X%s","size":0}}}}`,
					m, b64("IMG_0001.JPG"), time.Date(2023, 1, 2, 12, 0, 0, 0, time.Local).UnixMilli(), len(p), f.srv.URL, p, m),
				fmt.Sprintf(`{"recordName":%q,"recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":%q}}}}`, a, m))
		}
		fmt.Fprint(w, `{"records":[`+strings.Join(recs, ",")+`]}`)
	})
	mux.HandleFunc("POST /{guid}/sharedstreams/webgetassetcount", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n, ok := f.albumCounts[r.PathValue("guid")]
		f.mu.Unlock()
		if !ok {
			n = 1
		}
		fmt.Fprintf(w, `{"albumassetcount":%s}`, strconv.Itoa(n))
	})

	mux.HandleFunc("GET /content/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeICloud) client(t *testing.T) *icloud.Client {
	u := f.srv.URL
	return icloud.NewTestClient(t.TempDir(), "42", map[string]string{"drivews": u, "docws": u, "ckdatabasews": u, "sharedstreams": u}, nil)
}

func runServices(t *testing.T, dest string, services ...Service) map[string]*Stats {
	t.Helper()
	stats, err := Run(context.Background(), Options{Dest: dest, AppleID: "test@example.com", Workers: 2, Logf: t.Logf}, services)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestDriveServiceEndToEnd(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	st := runServices(t, dest, DriveService(f.client(t)))["drive"]
	if st.ListError != nil || st.Downloaded != 2 {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	want := map[string]string{
		"Drive/notes.txt":             "/content/drive/A",
		"Drive/Docs_ 2024/report.pdf": "/content/drive/B", // ':' is not allowed on exFAT
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tree %v", got)
	}
}

func TestPhotosServiceLayoutAndCount(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	svcs := PhotosServices(f.client(t), t.Logf)
	stats := runServices(t, dest, svcs[0], svcs[1])
	if st := stats["photos"]; st.ListError != nil || st.Downloaded != 4 {
		t.Fatalf("photos %+v", st)
	}
	if st := stats["photos-shared"]; st.ListError != nil || st.Listed != 0 {
		t.Fatalf("no shared library expected: %+v", st)
	}
	got := tree(t, dest)
	for p, body := range map[string]string{
		"Photos/Primary/2024/05/17/IMG_1.HEIC":         "/content/photos/m1",
		"Photos/Primary/2024/05/17/IMG_1_HEVC.MOV":     "/content/photos/m1live",
		"Photos/Primary/2024/05/17/clip.mov":           "/content/photos/m3",
		"Photos/Primary/unknown-date/a2-012345678.JPG": "/content/photos/m2",
	} {
		if got[p] != body {
			t.Errorf("%s = %q (tree %v)", p, got[p], got)
		}
	}

	// iCloud's count disagrees: the listing is treated as incomplete, so a
	// photo that vanished from the listing must not be deleted
	f.mu.Lock()
	f.photoCount = 99
	f.mu.Unlock()
	st := runServices(t, dest, svcs[0])["photos"]
	if st.ListError == nil || !strings.Contains(st.ListError.Error(), "counts 99") {
		t.Fatalf("mismatch not reported: %+v", st)
	}
}

func TestPhotosNotIndexedStillListsButVerifiesCount(t *testing.T) {
	f := newFakeICloud(t)
	f.indexState = "FAILED"
	dest := t.TempDir()
	st := runServices(t, dest, PhotosServices(f.client(t), t.Logf)[0])["photos"]
	if st.ListError != nil || st.Downloaded != 4 {
		t.Fatalf("matching count should make the listing trusted: %+v", st)
	}
	f.photoCount = 2
	st = runServices(t, dest, PhotosServices(f.client(t), t.Logf)[0])["photos"]
	if st.ListError == nil {
		t.Fatal("not indexed + count mismatch must be a listing error")
	}
}

func TestSharedAlbumsServiceEndToEnd(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	st := runServices(t, dest, SharedAlbumsService(f.client(t), t.Logf))["photos-albums"]
	if st.ListError != nil || st.Downloaded != 4 {
		t.Fatalf("%+v", st)
	}
	got := tree(t, dest)
	for p, body := range map[string]string{
		// same-name items within an album get a suffix
		"Photos/SharedAlbums/Trip/2023/01/02/IMG_0001.JPG":     "/content/albums/G1-m0",
		"Photos/SharedAlbums/Trip/2023/01/02/IMG_0001 (2).JPG": "/content/albums/G1-m1",
		// album names that differ only by case get distinct folders
		"Photos/SharedAlbums/trip (2)/2023/01/02/IMG_0001.JPG": "/content/albums/G2-m0",
		// a hostile album name cannot climb out of SharedAlbums/
		"Photos/SharedAlbums/.._/2023/01/02/IMG_0001.JPG": "/content/albums/G3-m0",
	} {
		if got[p] != body {
			t.Errorf("%s = %q", p, got[p])
		}
	}
	for p := range got {
		if !strings.HasPrefix(p, "Photos/SharedAlbums/") {
			t.Errorf("file escaped the albums folder: %s", p)
		}
	}
	if len(got) != 4 {
		t.Fatalf("tree %v", got)
	}

	// one album failing must not stop the others, and must block deletions
	f.mu.Lock()
	f.brokenAlbum = "G1"
	f.mu.Unlock()
	st = runServices(t, dest, SharedAlbumsService(f.client(t), t.Logf))["photos-albums"]
	if st.ListError == nil || st.Deleted != 0 || st.Skipped != 2 {
		t.Fatalf("%+v", st)
	}
	if len(tree(t, dest)) != 4 {
		t.Fatal("files were deleted after an incomplete listing")
	}

	// a count mismatch is an incomplete listing too
	f.mu.Lock()
	f.brokenAlbum = ""
	f.albumCounts["G2"] = 5
	f.mu.Unlock()
	st = runServices(t, dest, SharedAlbumsService(f.client(t), t.Logf))["photos-albums"]
	if st.ListError == nil || !strings.Contains(st.ListError.Error(), "trip") {
		t.Fatalf("%+v", st)
	}
}

func TestSharedAlbumsServiceUnavailable(t *testing.T) {
	c := icloud.NewTestClient(t.TempDir(), "42", nil, nil)
	st := runServices(t, t.TempDir(), SharedAlbumsService(c, t.Logf))["photos-albums"]
	if st.ListError != nil || st.Listed != 0 {
		t.Fatalf("%+v", st)
	}
}

// TestGoldenManifestKeys pins the manifest keys, paths and versions a plain run
// produces. About 39 GiB of real mirrors are recorded under exactly these, so a
// change that renames a key re-downloads (or deletes) everything: new flags must
// leave the default output untouched.
func TestGoldenManifestKeys(t *testing.T) {
	f := newFakeICloud(t)
	dest := t.TempDir()
	svcs := PhotosServices(f.client(t), t.Logf)
	runServices(t, dest, svcs[0], SharedAlbumsService(f.client(t), t.Logf))
	man, err := loadManifest(dest)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ path, version string }
	got := map[string]row{}
	for svc, entries := range man.Services {
		for k, e := range entries {
			got[svc+"|"+k] = row{e.Path, e.Version}
		}
	}
	want := map[string]row{
		"photos|a1":                  {"Photos/Primary/2024/05/17/IMG_1.HEIC", "Cm1"},
		"photos|a1#live":             {"Photos/Primary/2024/05/17/IMG_1_HEVC.MOV", "Cm1live"},
		"photos|a2-0123456789abcdef": {"Photos/Primary/unknown-date/a2-012345678.JPG", "Cm2"},
		"photos|a3":                  {"Photos/Primary/2024/05/17/clip.mov", "Cm3"},
		"photos-albums|G1/G1-a0":     {"Photos/SharedAlbums/Trip/2023/01/02/IMG_0001.JPG", "XG1-m0"},
		"photos-albums|G1/G1-a1":     {"Photos/SharedAlbums/Trip/2023/01/02/IMG_0001 (2).JPG", "XG1-m1"},
		"photos-albums|G2/G2-a0":     {"Photos/SharedAlbums/trip (2)/2023/01/02/IMG_0001.JPG", "XG2-m0"},
		"photos-albums|G3/G3-a0":     {"Photos/SharedAlbums/.._/2023/01/02/IMG_0001.JPG", "XG3-m0"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("manifest changed:\n got %v\nwant %v", got, want)
	}
}
