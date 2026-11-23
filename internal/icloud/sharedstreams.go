package icloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SharedAlbum is an iCloud "Shared Album" (Shared Streams), the ones created in
// or subscribed to from the Photos app. They are a separate service from the
// CloudKit photo libraries (sharedstreams.icloud.com).
type SharedAlbum struct {
	GUID        string
	Name        string
	Location    string // per-album base URL, ends with "/sharedstreams/"
	Ctag        string
	SharingType string // "owned" or "subscribed"
	Created     int64  // creation time, ms since the epoch (0 if unknown)
}

func (c *Client) sharedStreamsPost(ctx context.Context, rawURL string, body, out any) error {
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	_, err := Retry(ctx, func(err error, d time.Duration) {
		c.warnf("shared albums: request failed (%v), retrying in %s", err, d)
	}, func() (struct{}, error) {
		_, err := c.doJSON(ctx, http.MethodPost, rawURL, h, body, out)
		return struct{}{}, err
	})
	return err
}

// SharedAlbums lists the shared albums the account owns or subscribes to.
func (c *Client) SharedAlbums(ctx context.Context) ([]SharedAlbum, error) {
	base, err := c.ServiceURL("sharedstreams")
	if err != nil {
		return nil, err
	}
	p := c.Params()
	var resp struct {
		Albums []struct {
			GUID        string `json:"albumguid"`
			Location    string `json:"albumlocation"`
			Ctag        string `json:"albumctag"`
			SharingType string `json:"sharingtype"`
			Attributes  struct {
				Name         string      `json:"name"`
				CreationDate json.Number `json:"creationDate"`
			} `json:"attributes"`
		} `json:"albums"`
	}
	if err := c.sharedStreamsPost(ctx, base+"/"+p.Get("dsid")+"/sharedstreams/webgetalbumslist?"+p.Encode(), map[string]any{}, &resp); err != nil {
		return nil, fmt.Errorf("shared albums: %w", err)
	}
	out := make([]SharedAlbum, 0, len(resp.Albums))
	for _, a := range resp.Albums {
		if a.GUID == "" || !strings.HasPrefix(a.Location, "http") {
			return nil, fmt.Errorf("shared albums: malformed album entry %q", a.GUID)
		}
		loc := a.Location
		if !strings.HasSuffix(loc, "/") {
			loc += "/"
		}
		created, _ := strconv.ParseInt(a.Attributes.CreationDate.String(), 10, 64)
		out = append(out, SharedAlbum{GUID: a.GUID, Name: a.Attributes.Name, Location: loc, Ctag: a.Ctag, SharingType: a.SharingType, Created: created})
	}
	return out, nil
}

// SharedAlbumCount returns the number of items iCloud reports for an album.
func (c *Client) SharedAlbumCount(ctx context.Context, a SharedAlbum) (int, error) {
	var resp struct {
		Count json.Number `json:"albumassetcount"`
	}
	if err := c.sharedStreamsPost(ctx, a.Location+"webgetassetcount?"+c.Params().Encode(), map[string]any{"albumguid": a.GUID}, &resp); err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(resp.Count.String())
	if err != nil {
		return 0, fmt.Errorf("shared album count %q: %w", resp.Count, err)
	}
	return n, nil
}

// SharedAlbumPageSize is the number of items requested per webgetassets call.
// Each call takes tens of seconds whatever its size, so pages are large.
var SharedAlbumPageSize = 500

// SharedAlbumList pages through an album, calling fn for each page of items.
// It returns an error at the end if any item had to be skipped.
func (c *Client) SharedAlbumList(ctx context.Context, a SharedAlbum, fn func([]PhotoAsset) error) error {
	offset, skipped, badPages := 0, 0, 0
	prevMasters := map[string]ckRecord{}
	for {
		body := map[string]any{
			"albumguid": a.GUID,
			"albumctag": a.Ctag,
			"offset":    strconv.Itoa(offset),
			"limit":     strconv.Itoa(offset + SharedAlbumPageSize), // an end position, not a count
		}
		var resp ckQueryResponse
		if err := c.sharedStreamsPost(ctx, a.Location+"webgetassets?"+c.Params().Encode(), body, &resp); err != nil {
			return err
		}
		if resp.ServerErrorCode != "" {
			return fmt.Errorf("shared album: %s: %s", resp.ServerErrorCode, resp.Reason)
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
				return fmt.Errorf("shared album: %d unreadable records were skipped", skipped)
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
