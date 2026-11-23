package icloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const DriveRootID = "FOLDER::com.apple.CloudDocs::root"

type DriveItem struct {
	DrivewsID     string      `json:"drivewsid"`
	DocwsID       string      `json:"docwsid"`
	Zone          string      `json:"zone"`
	Name          string      `json:"name"`
	Extension     string      `json:"extension"`
	Type          string      `json:"type"` // FOLDER, APP_LIBRARY, FILE
	Size          int64       `json:"size"`
	DateModified  string      `json:"dateModified"`
	Etag          string      `json:"etag"`
	Items         []DriveItem `json:"items"`
	NumberOfItems *int        `json:"numberOfItems"`
}

func (d DriveItem) IsDir() bool { return d.Type == "FOLDER" || d.Type == "APP_LIBRARY" }

// FileName is the display name including extension.
func (d DriveItem) FileName() string {
	if d.Extension != "" {
		return d.Name + "." + d.Extension
	}
	return d.Name
}

// DriveFolders fetches full details (with children) for the given folder ids.
func (c *Client) DriveFolders(ctx context.Context, ids []string) ([]DriveItem, error) {
	base, err := c.ServiceURL("drivews")
	if err != nil {
		return nil, err
	}
	payload := make([]map[string]any, len(ids))
	for i, id := range ids {
		payload[i] = map[string]any{"drivewsid": id, "partialData": false}
	}
	var out []DriveItem
	u := base + "/retrieveItemDetailsInFolders?" + c.Params().Encode()
	if _, err := c.doJSON(ctx, http.MethodPost, u, nil, payload, &out); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("drive: asked for %d folders, got %d", len(ids), len(out))
	}
	// return the folders in request order (callers pair them by index) and
	// refuse entries without a child list: treating one as an empty folder
	// would make its files look deleted
	byID := make(map[string]DriveItem, len(out))
	for _, f := range out {
		byID[f.DrivewsID] = f
	}
	matched := 0
	for _, id := range ids {
		if _, ok := byID[id]; ok {
			matched++
		}
	}
	positional := matched == 0 // ids not echoed back at all: trust the order
	if !positional && matched != len(ids) {
		return nil, fmt.Errorf("drive: response does not match the %d requested folders", len(ids))
	}
	sorted := make([]DriveItem, len(ids))
	for i, id := range ids {
		f := byID[id]
		if positional {
			f = out[i]
		}
		if f.Items == nil && (f.NumberOfItems == nil || *f.NumberOfItems != 0) {
			return nil, fmt.Errorf("drive: folder %s returned no item list", id)
		}
		if f.NumberOfItems != nil && len(f.Items) < *f.NumberOfItems {
			return nil, fmt.Errorf("drive: folder %s returned %d of %d items", id, len(f.Items), *f.NumberOfItems)
		}
		sorted[i] = f
	}
	return sorted, nil
}

// DriveDownloadURL resolves the content URL for a file. isPackage is true for
// packages (.app, .textbundle, ...), whose content is a zip archive of the
// package directory.
func (c *Client) DriveDownloadURL(ctx context.Context, item DriveItem) (u string, isPackage bool, err error) {
	base, err := c.ServiceURL("docws")
	if err != nil {
		return "", false, err
	}
	zone := item.Zone
	if zone == "" {
		zone = "com.apple.CloudDocs"
	}
	p := c.Params()
	p.Set("document_id", item.DocwsID)
	var resp struct {
		DataToken *struct {
			URL string `json:"url"`
		} `json:"data_token"`
		PackageToken *struct {
			URL string `json:"url"`
		} `json:"package_token"`
	}
	endpoint := base + "/ws/" + url.PathEscape(zone) + "/download/by_id?" + p.Encode()
	if _, err := c.doJSON(ctx, http.MethodGet, endpoint, nil, nil, &resp); err != nil {
		return "", false, err
	}
	switch {
	case resp.DataToken != nil && resp.DataToken.URL != "":
		return resp.DataToken.URL, false, nil
	case resp.PackageToken != nil && resp.PackageToken.URL != "":
		return resp.PackageToken.URL, true, nil
	}
	return "", false, fmt.Errorf("drive: no download url for %s", item.FileName())
}

// Fetch GETs a content URL, merging the web-service params into its query
// (as pyicloud does). The caller closes the body.
func (c *Client) Fetch(ctx context.Context, rawURL string, withParams bool) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if withParams {
		// append without re-encoding the (possibly signed) existing query
		extra := c.Params().Encode()
		if u.RawQuery != "" {
			u.RawQuery += "&" + extra
		} else {
			u.RawQuery = extra
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		return nil, newAPIError(resp)
	}
	resp.Body = newIdleBody(resp.Body, cancel, DownloadIdleTimeout)
	return resp, nil
}

// DownloadIdleTimeout aborts a download that receives no data for this long
// (a dead connection would otherwise hang the run forever).
var DownloadIdleTimeout = 2 * time.Minute

// ErrStalled is returned by a download body that stopped receiving data.
var ErrStalled = errors.New("download stalled: no data received for a while")

type idleBody struct {
	rc      io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer
	timeout time.Duration
	stalled atomic.Bool
}

func newIdleBody(rc io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) *idleBody {
	b := &idleBody{rc: rc, cancel: cancel, timeout: timeout}
	b.timer = time.AfterFunc(time.Hour, func() {
		b.stalled.Store(true)
		cancel()
	})
	b.timer.Stop() // armed by the first Read: the caller may queue before reading
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.timeout)
	n, err := b.rc.Read(p)
	if err != nil && b.stalled.Load() {
		return n, ErrStalled
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}
