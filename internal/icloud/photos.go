package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type PhotoResource struct {
	URL      string
	Size     int64
	Checksum string
	FileType string
}

// PhotoSize names one rendition of a photo.
type PhotoSize string

const (
	SizeOriginal    PhotoSize = "original"
	SizeAdjusted    PhotoSize = "adjusted"    // the edited version (resJPEGFull)
	SizeAlternative PhotoSize = "alternative" // the other half of a RAW+JPEG pair
	SizeMedium      PhotoSize = "medium"
	SizeThumb       PhotoSize = "thumb"
)

// ParsePhotoSizes turns "original,adjusted" into a de-duplicated list.
func ParsePhotoSizes(list string) ([]PhotoSize, error) {
	var out []PhotoSize
	seen := map[PhotoSize]bool{}
	for _, f := range strings.Split(list, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		sz := PhotoSize(f)
		switch sz {
		case SizeOriginal, SizeAdjusted, SizeAlternative, SizeMedium, SizeThumb:
		default:
			return nil, fmt.Errorf("unknown size %q (use original, adjusted, alternative, medium, thumb)", f)
		}
		if !seen[sz] {
			seen[sz] = true
			out = append(out, sz)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no size given")
	}
	return out, nil
}

type PhotoAsset struct {
	ID       string // CPLAsset recordName (unique per library item)
	MasterID string
	Filename string
	ItemType string
	Date     time.Time
	Original *PhotoResource
	Live     *PhotoResource // video part of a Live Photo
	// Versions holds the optional renditions the listing returned (adjusted,
	// alternative, medium, thumb); photos only. They are present only when the
	// listing asked for them (ListOptions.Sizes).
	Versions map[PhotoSize]*PhotoResource
	// Meta is the descriptive metadata (caption, keywords, location, favorite),
	// filled when the listing asked for it (ListOptions.Meta).
	Meta *PhotoMeta
}

type ckField struct {
	Value json.RawMessage `json:"value"`
	Type  string          `json:"type"`
}

type ckRecord struct {
	RecordName      string             `json:"recordName"`
	RecordType      string             `json:"recordType"`
	Fields          map[string]ckField `json:"fields"`
	ServerErrorCode string             `json:"serverErrorCode"`
	Reason          string             `json:"reason"`
}

type ckQueryResponse struct {
	Records         []ckRecord `json:"records"`
	ServerErrorCode string     `json:"serverErrorCode"`
	Reason          string     `json:"reason"`
}

func (c *Client) photosEndpoint() (string, error) {
	base, err := c.ServiceURL("ckdatabasews")
	if err != nil {
		return "", err
	}
	return base + "/database/1/com.apple.photos.cloud/production/private", nil
}

func (c *Client) photosQuery(ctx context.Context, body any) (*ckQueryResponse, error) {
	ep, err := c.photosEndpoint()
	if err != nil {
		return nil, err
	}
	p := c.Params()
	p.Set("remapEnums", "true")
	p.Set("getCurrentSyncToken", "true")
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	var out ckQueryResponse
	if _, err := c.doJSON(ctx, http.MethodPost, ep+"/records/query?"+p.Encode(), h, body, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "ZONE_NOT_FOUND" || apiErr.Code == "AUTHENTICATION_FAILED") {
			return nil, fmt.Errorf("%w (finish setting up iCloud Photos at icloud.com)", err)
		}
		if errors.As(err, &apiErr) && strings.Contains(strings.ToUpper(apiErr.Message+apiErr.Code), "PCS") {
			return nil, fmt.Errorf("%w (Advanced Data Protection: enable \"Access iCloud Data on the Web\")", err)
		}
		return nil, err
	}
	if out.ServerErrorCode != "" {
		return nil, fmt.Errorf("photos: %s: %s", out.ServerErrorCode, out.Reason)
	}
	return &out, nil
}

// photosZones lists the private photo zones (zoneID objects), skipping deleted/broken ones.
func (c *Client) photosZones(ctx context.Context) ([]map[string]any, error) {
	ep, err := c.photosEndpoint()
	if err != nil {
		return nil, err
	}
	p := c.Params()
	p.Set("remapEnums", "true")
	p.Set("getCurrentSyncToken", "true")
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	var out struct {
		Zones []struct {
			ZoneID          map[string]any `json:"zoneID"`
			Deleted         bool           `json:"deleted"`
			ServerErrorCode string         `json:"serverErrorCode"`
		} `json:"zones"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, ep+"/zones/list?"+p.Encode(), h, "{}", &out); err != nil {
		return nil, err
	}
	var zones []map[string]any
	for _, z := range out.Zones {
		name, _ := z.ZoneID["zoneName"].(string)
		c.tracef("photos: zone %q deleted=%v error=%q", name, z.Deleted, z.ServerErrorCode)
		if name == "" || z.Deleted || z.ServerErrorCode != "" {
			continue
		}
		zones = append(zones, z.ZoneID)
	}
	return zones, nil
}

func (c *Client) indexState(ctx context.Context, zone map[string]any) (string, error) {
	body := map[string]any{
		"query":  map[string]any{"recordType": "CheckIndexingState"},
		"zoneID": zone,
	}
	resp, err := c.photosQuery(ctx, body)
	if err != nil {
		return "", err
	}
	if len(resp.Records) == 0 {
		return "", errors.New("photos: empty indexing state response")
	}
	var state string
	if f, ok := resp.Records[0].Fields["state"]; ok {
		_ = json.Unmarshal(f.Value, &state)
	}
	if raw, err := json.Marshal(resp.Records[0].Fields); err == nil {
		c.tracef("photos: indexing state for %v: %s", zone["zoneName"], raw)
	}
	return state, nil
}

// PhotoLibrary is one photo zone: the personal library or the iCloud Shared Photo Library.
type PhotoLibrary struct {
	Kind string // "primary" or "shared"
	Zone map[string]any
}

func (l PhotoLibrary) ZoneName() string { n, _ := l.Zone["zoneName"].(string); return n }

// PhotoLibraries finds the personal library zone (PrimarySync, or PrimarySync1 etc.
// on accounts where Apple moved it) and any shared library zones (SharedSync-...).
func (c *Client) PhotoLibraries(ctx context.Context) ([]PhotoLibrary, error) {
	zones, err := c.photosZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("photos: could not list libraries: %w", err)
	}
	var primary []map[string]any
	var libs []PhotoLibrary
	for _, z := range zones {
		name, _ := z["zoneName"].(string)
		switch {
		case name == "PrimarySync":
			primary = append([]map[string]any{z}, primary...)
		case strings.HasPrefix(name, "PrimarySync"):
			primary = append(primary, z)
		case strings.HasPrefix(name, "SharedSync"):
			libs = append(libs, PhotoLibrary{Kind: "shared", Zone: z})
		}
	}
	if len(primary) == 0 {
		names := make([]string, 0, len(zones))
		for _, z := range zones {
			names = append(names, fmt.Sprint(z["zoneName"]))
		}
		return nil, fmt.Errorf("photos: no photo library zone found (zones: %s); is iCloud Photos enabled?", strings.Join(names, ", "))
	}
	return append([]PhotoLibrary{{Kind: "primary", Zone: primary[0]}}, libs...), nil
}

// PhotosReady fails with *PhotosNotReadyError unless Apple reports lib as indexed.
func (c *Client) PhotosReady(ctx context.Context, lib PhotoLibrary) error {
	var state string
	for attempt := 1; attempt <= 3; attempt++ {
		st, err := c.indexState(ctx, lib.Zone)
		if err != nil {
			return err
		}
		state = st
		if state != "RUNNING" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	if state == "FINISHED" {
		return nil
	}
	return &PhotosNotReadyError{States: fmt.Sprintf("%s=%s", lib.ZoneName(), state)}
}

// allPhotosObjType is the index "All Photos" (without hidden/deleted) is counted by.
const allPhotosObjType = "CPLAssetByAssetDateWithoutHiddenOrDeleted"

// PhotosCount returns how many items "All Photos" holds, per Apple's index.
func (c *Client) PhotosCount(ctx context.Context, lib PhotoLibrary) (int, error) {
	return c.countObjType(ctx, lib, allPhotosObjType)
}

func (c *Client) countObjType(ctx context.Context, lib PhotoLibrary, objType string) (int, error) {
	ep, err := c.photosEndpoint()
	if err != nil {
		return 0, err
	}
	p := c.Params()
	p.Set("remapEnums", "true")
	p.Set("getCurrentSyncToken", "true")
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	body := map[string]any{"batch": []any{map[string]any{
		"resultsLimit": 1,
		"query": map[string]any{
			"filterBy": map[string]any{
				"fieldName":  "indexCountID",
				"fieldValue": map[string]any{"type": "STRING_LIST", "value": []string{objType}},
				"comparator": "IN",
			},
			"recordType": "HyperionIndexCountLookup",
		},
		"zoneWide": true,
		"zoneID":   lib.Zone,
	}}}
	var out struct {
		Batch []ckQueryResponse `json:"batch"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, ep+"/internal/records/query/batch?"+p.Encode(), h, body, &out); err != nil {
		return 0, err
	}
	if len(out.Batch) == 0 || len(out.Batch[0].Records) == 0 {
		return 0, errors.New("photos: empty count response")
	}
	var n int
	if f, ok := out.Batch[0].Records[0].Fields["itemCount"]; ok {
		if err := json.Unmarshal(f.Value, &n); err != nil {
			return 0, fmt.Errorf("photos: bad itemCount: %w", err)
		}
		return n, nil
	}
	return 0, errors.New("photos: count missing in response")
}

// PhotosNotReadyError: Apple does not report the library as indexed.
type PhotosNotReadyError struct{ States string }

func (e *PhotosNotReadyError) Error() string {
	return fmt.Sprintf("photos: iCloud reports the library as not indexed (%s)", e.States)
}

var photoKeys = []string{
	"recordName", "recordType", "masterRef", "assetDate", "addedDate", "isDeleted", "isHidden",
	"filenameEnc", "itemType",
	"resOriginalRes", "resOriginalFileType",
	"resOriginalVidComplRes", "resOriginalVidComplFileType",
}

// extra keys, requested only when a feature needs them so that a plain run
// sends exactly the request it always did
var (
	sizeKeys = []string{
		"resJPEGFullRes", "resJPEGFullFileType",
		"resOriginalAltRes", "resOriginalAltFileType",
		"resJPEGMedRes", "resJPEGMedFileType",
		"resJPEGThumbRes", "resJPEGThumbFileType",
	}
	metaKeys = []string{
		"isFavorite", "captionEnc", "extendedDescEnc", "keywordsEnc", "locationEnc",
		"timeZoneOffset", "assetSubtypeV2",
	}
)

// maxUnreadablePages bounds how many consecutive pages without a single
// readable asset a listing walks through before giving up.
const maxUnreadablePages = 20

// ListOptions tunes a photo listing. The zero value is the plain listing.
type ListOptions struct {
	Sizes     bool   // also fetch the adjusted / alternative / medium / thumb renditions
	Meta      bool   // also fetch caption, keywords, location and favorite flag
	Direction string // "ASCENDING" (default) or "DESCENDING"
}

func (o ListOptions) keys() []string {
	keys := append([]string(nil), photoKeys...)
	if o.Sizes {
		keys = append(keys, sizeKeys...)
	}
	if o.Meta {
		keys = append(keys, metaKeys...)
	}
	return keys
}

// PhotosList pages through "All Photos" (without hidden/deleted) in ascending
// date order, calling fn for each page. Each page's download URLs are fresh.
func (c *Client) PhotosList(ctx context.Context, lib PhotoLibrary, fn func([]PhotoAsset) error) error {
	return c.PhotosListWith(ctx, lib, ListOptions{}, fn)
}

// PhotosListWith is PhotosList with options.
func (c *Client) PhotosListWith(ctx context.Context, lib PhotoLibrary, opts ListOptions, fn func([]PhotoAsset) error) error {
	return c.listAssets(ctx, lib, "CPLAssetAndMasterByAssetDateWithoutHiddenOrDeleted", nil, opts, fn)
}

// listAssets pages through one CloudKit asset query: the whole library or one album.
func (c *Client) listAssets(ctx context.Context, lib PhotoLibrary, recordType string, extraFilter []any, opts ListOptions, fn func([]PhotoAsset) error) error {
	const pageSize = 100
	direction := opts.Direction
	if direction == "" {
		direction = "ASCENDING"
	}
	offset := 0
	prevMasters := map[string]ckRecord{}
	skipped, badPages := 0, 0
	for {
		filter := []any{
			map[string]any{"fieldName": "startRank", "fieldValue": map[string]any{"type": "INT64", "value": offset}, "comparator": "EQUALS"},
			map[string]any{"fieldName": "direction", "fieldValue": map[string]any{"type": "STRING", "value": direction}, "comparator": "EQUALS"},
		}
		body := map[string]any{
			"query": map[string]any{
				"filterBy":   append(filter, extraFilter...),
				"recordType": recordType,
			},
			"resultsLimit": pageSize * 2,
			"desiredKeys":  opts.keys(),
			"zoneID":       lib.Zone,
		}
		resp, err := Retry(ctx, func(err error, d time.Duration) {
			c.warnf("photos: page at item %d failed (%v), retrying in %s", offset, err, d)
		}, func() (*ckQueryResponse, error) { return c.photosQuery(ctx, body) })
		if err != nil {
			return err
		}
		page, masters, n, bad := c.assemblePage(resp.Records, prevMasters)
		skipped += bad
		if len(page) == 0 && n > 0 {
			if badPages++; badPages > maxUnreadablePages {
				return fmt.Errorf("listing stopped after %d pages of unreadable records", badPages)
			}
		} else {
			badPages = 0
		}
		if n == 0 {
			if skipped > 0 {
				// downloads went ahead, but the listing is not trustworthy for deletions
				return fmt.Errorf("photos: %d unreadable records were skipped", skipped)
			}
			return nil
		}
		offset += n
		prevMasters = masters
		if err := fn(page); err != nil {
			return err
		}
	}
}

// assemblePage pairs the CPLAsset records of one query page with their
// CPLMaster records (a master can arrive on the previous page). It returns the
// usable assets, this page's masters, the number of asset ranks the page
// covered (the pagination step, unreadable assets included) and how many
// records had to be skipped.
func (c *Client) assemblePage(records []ckRecord, prevMasters map[string]ckRecord) ([]PhotoAsset, map[string]ckRecord, int, int) {
	var assets []ckRecord
	masters := map[string]ckRecord{}
	skipped, errAssets := 0, 0
	for _, r := range records {
		if r.ServerErrorCode != "" {
			skipped++
			if r.RecordType != "CPLMaster" {
				errAssets++ // an unreadable asset still occupies a rank
			}
			c.tracef("photos: record error %s: %s", r.ServerErrorCode, r.Reason)
			continue
		}
		switch r.RecordType {
		case "CPLAsset":
			assets = append(assets, r)
		case "CPLMaster":
			masters[r.RecordName] = r
		}
	}
	page := make([]PhotoAsset, 0, len(assets))
	for _, a := range assets {
		masterID := refName(a.Fields["masterRef"])
		m, ok := masters[masterID]
		if !ok {
			m, ok = prevMasters[masterID]
		}
		if !ok {
			skipped++
			c.tracef("photos: master record %s for asset %s missing, skipped", masterID, a.RecordName)
			continue
		}
		pa, err := buildAsset(a, m)
		if err != nil {
			skipped++
			c.tracef("photos: asset %s skipped: %v", a.RecordName, err)
			continue
		}
		page = append(page, pa)
	}
	return page, masters, len(assets) + errAssets, skipped
}

func hasAny(fields map[string]ckField, keys ...string) bool {
	for _, k := range keys {
		if _, ok := fields[k]; ok {
			return true
		}
	}
	return false
}

func refName(f ckField) string {
	var v struct {
		RecordName string `json:"recordName"`
	}
	_ = json.Unmarshal(f.Value, &v)
	return v.RecordName
}

func fieldString(fields map[string]ckField, key string) string {
	var s string
	if f, ok := fields[key]; ok {
		_ = json.Unmarshal(f.Value, &s)
	}
	return s
}

func resource(asset, master ckRecord, prefix string) (*PhotoResource, error) {
	fields := asset.Fields
	if _, ok := fields[prefix+"Res"]; !ok {
		fields = master.Fields
	}
	f, ok := fields[prefix+"Res"]
	if !ok {
		return nil, nil
	}
	var v struct {
		Size        int64  `json:"size"`
		DownloadURL string `json:"downloadURL"`
		Checksum    string `json:"fileChecksum"`
	}
	if err := json.Unmarshal(f.Value, &v); err != nil {
		return nil, fmt.Errorf("photos: bad %sRes on %s: %w", prefix, master.RecordName, err)
	}
	if v.DownloadURL == "" {
		return nil, fmt.Errorf("photos: no download URL for %s", master.RecordName)
	}
	if v.Size == 0 { // shared albums report the size in a separate field
		if f, ok := fields[prefix+"FileSize"]; ok {
			var n float64
			if json.Unmarshal(f.Value, &n) == nil {
				v.Size = int64(n)
			}
		}
	}
	return &PhotoResource{URL: v.DownloadURL, Size: v.Size, Checksum: v.Checksum, FileType: fieldString(fields, prefix+"FileType")}, nil
}

func buildAsset(a, m ckRecord) (PhotoAsset, error) {
	pa := PhotoAsset{ID: a.RecordName, MasterID: m.RecordName, ItemType: fieldString(m.Fields, "itemType")}
	if pa.ItemType == "" { // shared albums carry only the resource type
		pa.ItemType = fieldString(m.Fields, "resOriginalFileType")
	}
	if f, ok := m.Fields["filenameEnc"]; ok {
		var s string
		_ = json.Unmarshal(f.Value, &s)
		if f.Type == "ENCRYPTED_BYTES" || f.Type == "BYTES" {
			if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
				s = string(dec)
			}
		}
		pa.Filename = s
	}
	// private libraries: assetDate; shared albums: the master's creation date,
	// then the date it was added to the album
	for _, f := range []ckField{a.Fields["assetDate"], m.Fields["originalCreationDate"], a.Fields["addedDate"]} {
		var ms float64
		if len(f.Value) > 0 && json.Unmarshal(f.Value, &ms) == nil && ms != 0 {
			pa.Date = time.UnixMilli(int64(ms))
			break
		}
	}
	var err error
	if pa.Original, err = resource(a, m, "resOriginal"); err != nil {
		return pa, err
	}
	// Shared-album videos come as resVidFull/Med/Small renditions; their
	// "original" may be just a poster frame, so prefer the best video rendition.
	if pa.Original == nil || !IsMovieType(pa.Original.FileType) {
		for _, p := range []string{"resVidFull", "resVidMed", "resVidSmall"} {
			v, err := resource(a, m, p)
			if err != nil {
				return pa, err
			}
			if v != nil {
				pa.Original, pa.ItemType = v, v.FileType
				break
			}
		}
	}
	if pa.Original == nil {
		return pa, fmt.Errorf("photos: asset %s has no original", a.RecordName)
	}
	if !IsMovieType(pa.ItemType) {
		if pa.Live, err = resource(a, m, "resOriginalVidCompl"); err != nil {
			return pa, err
		}
		// optional renditions: one that cannot be read is simply absent, it must
		// not make the whole photo unreadable
		for size, prefix := range map[PhotoSize]string{
			SizeAdjusted: "resJPEGFull", SizeAlternative: "resOriginalAlt",
			SizeMedium: "resJPEGMed", SizeThumb: "resJPEGThumb",
		} {
			if r, rerr := resource(a, m, prefix); rerr == nil && r != nil {
				if pa.Versions == nil {
					pa.Versions = map[PhotoSize]*PhotoResource{}
				}
				pa.Versions[size] = r
			}
		}
	}
	if _, ok := a.Fields["captionEnc"]; ok || hasAny(a.Fields, "keywordsEnc", "locationEnc", "isFavorite", "extendedDescEnc", "assetSubtypeV2") {
		pa.Meta = buildMeta(a)
	}
	return pa, nil
}

var itemTypeExt = map[string]string{
	"public.heic": "HEIC", "public.heif": "HEIF", "public.jpeg": "JPG", "public.png": "PNG",
	"public.jpeg-xl": "JXL", "com.compuserve.gif": "GIF", "public.tiff": "TIFF",
	"com.apple.quicktime-movie": "MOV", "public.mpeg-4": "MP4", "com.adobe.raw-image": "DNG",
	"org.webmproject.webp": "WEBP",
}

func IsMovieType(t string) bool {
	return t == "com.apple.quicktime-movie" || t == "public.mpeg-4" || strings.Contains(t, "movie") || strings.Contains(t, "video")
}

// ItemTypeExt returns a file extension for a UTI, or "bin".
func ItemTypeExt(t string) string {
	if e, ok := itemTypeExt[t]; ok {
		return e
	}
	return "bin"
}
