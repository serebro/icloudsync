package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
)

func asset(id, master string, dateMs int64) string {
	return fmt.Sprintf(`{"recordName":%q,"recordType":"CPLAsset","fields":{"masterRef":{"type":"REFERENCE","value":{"recordName":%q}},"assetDate":{"type":"TIMESTAMP","value":%d}}}`, id, master, dateMs)
}

func master(id, name, itemType string, live bool, srv string) string {
	live_ := ""
	if live {
		live_ = fmt.Sprintf(`,"resOriginalVidComplRes":{"value":{"size":5,"downloadURL":"%s/dl/%s-live","fileChecksum":"L%s"}},"resOriginalVidComplFileType":{"value":"com.apple.quicktime-movie"}`, srv, id, id)
	}
	return fmt.Sprintf(`{"recordName":%q,"recordType":"CPLMaster","fields":{"filenameEnc":{"type":"ENCRYPTED_BYTES","value":%q},"itemType":{"value":%q},"resOriginalRes":{"value":{"size":3,"downloadURL":"%s/dl/%s","fileChecksum":"C%s"}},"resOriginalFileType":{"value":%q}%s}}`,
		id, base64.StdEncoding.EncodeToString([]byte(name)), itemType, srv, id, id, itemType, live_)
}

func TestPhotosPagination(t *testing.T) {
	var srvURL string
	var ranks []int
	failedOnce := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /database/1/com.apple.photos.cloud/production/private/zones/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"zones":[{"zoneID":{"zoneName":"SharedSync-ABC","ownerRecordName":"_o"}},{"zoneID":{"zoneName":"PrimarySync"}},{"zoneID":{"zoneName":"Old"},"deleted":true}]}`)
	})
	mux.HandleFunc("POST /database/1/com.apple.photos.cloud/production/private/records/query", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dsid") != "42" || r.URL.Query().Get("remapEnums") != "true" {
			t.Errorf("params: %s", r.URL.RawQuery)
		}
		var q struct {
			Query struct {
				RecordType string
				FilterBy   []struct {
					FieldName  string
					FieldValue struct{ Value any }
				}
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("Content-Type", "application/json")
		if q.Query.RecordType == "CheckIndexingState" {
			fmt.Fprint(w, `{"records":[{"recordType":"CheckIndexingState","fields":{"state":{"value":"FINISHED"}}}]}`)
			return
		}
		rank := int(q.Query.FilterBy[0].FieldValue.Value.(float64))
		ranks = append(ranks, rank)
		if rank == 2 && !failedOnce { // transient outage mid-listing must not abort it
			failedOnce = true
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch rank {
		case 0: // two assets; a duplicate shares master m1
			fmt.Fprintf(w, `{"records":[%s,%s,%s]}`, asset("a1", "m1", 1700000000000), master("m1", "IMG_1.HEIC", "public.heic", true, srvURL), asset("a2", "m1", 1700000000000))
		case 2: // master of a3 was on the previous page? no: m2 comes here
			fmt.Fprintf(w, `{"records":[%s,%s]}`, asset("a3", "m2", 1700000100000), master("m2", "clip.mov", "com.apple.quicktime-movie", false, srvURL))
		default:
			fmt.Fprint(w, `{"records":[]}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	c := newTestClient(t, filepath.Join(t.TempDir(), "s.json"), nil)
	acc := &AccountData{}
	acc.DsInfo.Dsid = "42"
	acc.Webservices = map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	}{"ckdatabasews": {URL: srv.URL}}
	c.setAccount(acc)

	libs, err := c.PhotoLibraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 || libs[0].Kind != "primary" || libs[0].ZoneName() != "PrimarySync" || libs[1].Kind != "shared" || libs[1].Zone["ownerRecordName"] != "_o" {
		t.Fatalf("libraries %+v", libs)
	}
	if err := c.PhotosReady(context.Background(), libs[0]); err != nil {
		t.Fatal(err)
	}
	var got []PhotoAsset
	if err := c.PhotosList(context.Background(), libs[0], func(p []PhotoAsset) error { got = append(got, p...); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ranks) != "[0 2 2 3]" {
		t.Fatalf("startRank sequence %v", ranks)
	}
	if len(got) != 3 {
		t.Fatalf("got %d assets", len(got))
	}
	if got[0].Filename != "IMG_1.HEIC" || got[0].Live == nil || got[0].Original.Checksum != "Cm1" {
		t.Fatalf("asset 1: %+v", got[0])
	}
	if got[1].ID != "a2" || got[1].MasterID != "m1" {
		t.Fatalf("duplicate asset lost: %+v", got[1])
	}
	if got[2].Live != nil || got[2].Filename != "clip.mov" {
		t.Fatalf("movie: %+v", got[2])
	}
}

func TestDriveListAndDownload(t *testing.T) {
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /retrieveItemDetailsInFolders", func(w http.ResponseWriter, r *http.Request) {
		var req []struct{ Drivewsid string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if len(req) != 1 || req[0].Drivewsid != DriveRootID {
			t.Errorf("request %+v", req)
		}
		fmt.Fprint(w, `[{"drivewsid":"FOLDER::com.apple.CloudDocs::root","items":[
			{"drivewsid":"FILE::com.apple.CloudDocs::X","docwsid":"X","zone":"com.apple.CloudDocs","name":"report","extension":"pdf","type":"FILE","size":4,"etag":"e1","dateModified":"2024-01-02T03:04:05Z"},
			{"drivewsid":"FOLDER::com.apple.CloudDocs::D","name":"Docs","type":"FOLDER"}]}]`)
	})
	mux.HandleFunc("GET /ws/com.apple.CloudDocs/download/by_id", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("document_id") != "X" {
			t.Errorf("document_id %q", r.URL.Query().Get("document_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data_token":{"url":"%s/content/X?sig=a%%2Bb"}}`, srvURL)
	})
	mux.HandleFunc("GET /content/X", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery[:9] != "sig=a%2Bb" {
			t.Errorf("signed query re-encoded: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, "data")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	c := newTestClient(t, filepath.Join(t.TempDir(), "s.json"), nil)
	acc := &AccountData{}
	acc.DsInfo.Dsid = "42"
	acc.Webservices = map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	}{"drivews": {URL: srv.URL}, "docws": {URL: srv.URL}}
	c.setAccount(acc)

	folders, err := c.DriveFolders(context.Background(), []string{DriveRootID})
	if err != nil {
		t.Fatal(err)
	}
	items := folders[0].Items
	if len(items) != 2 || items[0].FileName() != "report.pdf" || !items[1].IsDir() {
		t.Fatalf("items %+v", items)
	}
	u, _, err := c.DriveDownloadURL(context.Background(), items[0])
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Fetch(context.Background(), u, true)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "data" {
		t.Fatalf("body %q", body)
	}
}

func TestSharedAlbums(t *testing.T) {
	var srvURL string
	// shared-stream records: BYTES filename, size only in resOriginalFileSize,
	// date on the master, no itemType
	ssMaster := func(id, name string) string {
		return fmt.Sprintf(`{"recordName":%q,"recordType":"CPLMaster","fields":{"filenameEnc":{"type":"BYTES","value":%q},"originalCreationDate":{"type":"TIMESTAMP","value":1474128681000},"resOriginalFileSize":{"type":"NUMBER_INT64","value":7},"resOriginalFileType":{"type":"STRING","value":"public.jpeg"},"resOriginalRes":{"type":"ASSETID","value":{"downloadURL":"%s/dl/%s","fileChecksum":"C%s","size":0}}}}`,
			id, base64.StdEncoding.EncodeToString([]byte(name)), srvURL, id, id)
	}
	ssAsset := func(id, master string) string {
		return fmt.Sprintf(`{"recordName":%q,"recordType":"CPLAsset","fields":{"masterRef":{"type":"REFERENCE","value":{"recordName":%q}},"addedDate":{"type":"TIMESTAMP","value":1700000000000}}}`, id, master)
	}
	var offsets []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /42/sharedstreams/webgetalbumslist", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"albums":[{"albumguid":"G1","albumlocation":"%s/G1/sharedstreams/","albumctag":"c1","sharingtype":"subscribed","attributes":{"name":"Trip"}}]}`, srvURL)
	})
	mux.HandleFunc("POST /G1/sharedstreams/webgetassets", func(w http.ResponseWriter, r *http.Request) {
		var q map[string]string
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q["albumguid"] != "G1" || q["albumctag"] != "c1" || q["limit"] != fmt.Sprint(atoi(q["offset"])+2) {
			t.Errorf("assets request %v", q)
		}
		offsets = append(offsets, q["offset"])
		switch q["offset"] {
		case "0":
			fmt.Fprintf(w, `{"records":[%s,%s,%s,%s]}`, ssMaster("m1", "IMG_1.JPG"), ssMaster("m2", "IMG_2.JPG"), ssAsset("a1", "m1"), ssAsset("a2", "m2"))
		case "2":
			fmt.Fprintf(w, `{"records":[%s,%s]}`, ssMaster("m3", "IMG_3.JPG"), ssAsset("a3", "m3"))
		default:
			fmt.Fprint(w, `{"records":[]}`)
		}
	})
	mux.HandleFunc("POST /G1/sharedstreams/webgetassetcount", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"albumassetcount":3}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	old := SharedAlbumPageSize
	SharedAlbumPageSize = 2
	defer func() { SharedAlbumPageSize = old }()
	c := newTestClient(t, filepath.Join(t.TempDir(), "s.json"), nil)
	acc := &AccountData{}
	acc.DsInfo.Dsid = "42"
	acc.Webservices = map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	}{"sharedstreams": {URL: srv.URL}}
	c.setAccount(acc)
	albums, err := c.SharedAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Name != "Trip" {
		t.Fatalf("albums %+v", albums)
	}
	var got []PhotoAsset
	if err := c.SharedAlbumList(context.Background(), albums[0], func(p []PhotoAsset) error { got = append(got, p...); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(offsets) != "[0 2 3]" {
		t.Fatalf("offsets %v", offsets)
	}
	if len(got) != 3 || got[0].Filename != "IMG_1.JPG" || got[0].Original.Size != 7 || got[0].Date.UnixMilli() != 1474128681000 || got[0].ItemType != "public.jpeg" {
		t.Fatalf("assets %+v", got)
	}
	n, err := c.SharedAlbumCount(context.Background(), albums[0])
	if err != nil || n != 3 {
		t.Fatalf("count %d %v", n, err)
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func TestSharedAlbumVideoRendition(t *testing.T) {
	var a, m ckRecord
	if err := json.Unmarshal([]byte(`{"recordName":"a","recordType":"CPLAsset","fields":{"masterRef":{"value":{"recordName":"m"}}}}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"recordName":"m","recordType":"CPLMaster","fields":{
		"filenameEnc":{"type":"BYTES","value":"Y2xpcC5NT1Y="},
		"resOriginalFileType":{"value":"public.jpeg"},"resOriginalRes":{"value":{"downloadURL":"https://x/poster","size":10}},
		"resVidSmallFileType":{"value":"com.apple.quicktime-movie"},"resVidSmallRes":{"value":{"downloadURL":"https://x/small","size":20}},
		"resVidMedFileType":{"value":"com.apple.quicktime-movie"},"resVidMedRes":{"value":{"downloadURL":"https://x/med","size":0}},"resVidMedFileSize":{"value":30}}}`), &m); err != nil {
		t.Fatal(err)
	}
	pa, err := buildAsset(a, m)
	if err != nil {
		t.Fatal(err)
	}
	if pa.Filename != "clip.MOV" || pa.Original.URL != "https://x/med" || pa.Original.Size != 30 || !IsMovieType(pa.ItemType) || pa.Live != nil {
		t.Fatalf("%+v %+v", pa, pa.Original)
	}
}

func TestIsTransientCloudKit(t *testing.T) {
	if !IsTransient(&APIError{Status: 409, Code: "TRY_AGAIN_LATER", Message: "Failed to provide read-consistent view"}) {
		t.Fatal("TRY_AGAIN_LATER must be retried")
	}
	if IsTransient(&APIError{Status: 409, Code: "CONFLICT"}) || IsTransient(&APIError{Status: 401}) {
		t.Fatal("non-retryable errors treated as transient")
	}
}
