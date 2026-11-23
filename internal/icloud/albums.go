package icloud

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// folderAlbumType is the albumType of a folder that groups albums.
const folderAlbumType = 3

// PhotoAlbum is a user album (or an album inside folders) of the personal library.
type PhotoAlbum struct {
	ID    string   // CloudKit recordName of the album
	Names []string // folder names and the album name: ["Folder", "Subfolder", "Album"]
	Lib   PhotoLibrary
}

// Path is the names joined for display and for --album matching: "Folder/Album".
func (a PhotoAlbum) Path() string { return strings.Join(a.Names, "/") }

// Name is the album's own name, without its folders.
func (a PhotoAlbum) Name() string { return a.Names[len(a.Names)-1] }

func (c *Client) queryAlbumFolders(ctx context.Context, lib PhotoLibrary, parentID string) ([]ckRecord, error) {
	ep, err := c.photosEndpoint()
	if err != nil {
		return nil, err
	}
	p := c.Params()
	p.Set("remapEnums", "true")
	p.Set("getCurrentSyncToken", "true")
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	query := map[string]any{"recordType": "CPLAlbumByPositionLive"}
	if parentID != "" {
		query["filterBy"] = []any{map[string]any{
			"fieldName": "parentId", "comparator": "EQUALS",
			"fieldValue": map[string]any{"type": "STRING", "value": parentID},
		}}
	}
	body := map[string]any{"query": query, "zoneID": lib.Zone}
	var records []ckRecord
	markers := map[string]bool{}
	for {
		var out struct {
			Records            []ckRecord `json:"records"`
			ContinuationMarker string     `json:"continuationMarker"`
			ServerErrorCode    string     `json:"serverErrorCode"`
			Reason             string     `json:"reason"`
		}
		_, err := Retry(ctx, func(err error, d time.Duration) {
			c.warnf("photos: album list failed (%v), retrying in %s", err, d)
		}, func() (struct{}, error) {
			out = struct {
				Records            []ckRecord `json:"records"`
				ContinuationMarker string     `json:"continuationMarker"`
				ServerErrorCode    string     `json:"serverErrorCode"`
				Reason             string     `json:"reason"`
			}{}
			_, err := c.doJSON(ctx, http.MethodPost, ep+"/records/query?"+p.Encode(), h, body, &out)
			return struct{}{}, err
		})
		if err != nil {
			return nil, err
		}
		if out.ServerErrorCode != "" {
			return nil, fmt.Errorf("photos: albums: %s: %s", out.ServerErrorCode, out.Reason)
		}
		records = append(records, out.Records...)
		// a marker seen before would only repeat the same page
		if out.ContinuationMarker == "" || markers[out.ContinuationMarker] {
			return records, nil
		}
		markers[out.ContinuationMarker] = true
		body["continuationMarker"] = out.ContinuationMarker
	}
}

// PhotoAlbums lists the user albums of a personal library, walking into
// folders one level at a time. Albums inside folders get the path
// "Folder/Album". Shared libraries have no albums.
func (c *Client) PhotoAlbums(ctx context.Context, lib PhotoLibrary) ([]PhotoAlbum, error) {
	if lib.Kind != "primary" {
		return nil, nil
	}
	type pending struct {
		parent string
		names  []string
	}
	queue := []pending{{"", nil}}
	visited := map[string]bool{}
	var albums []PhotoAlbum
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		recs, err := c.queryAlbumFolders(ctx, lib, cur.parent)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.ServerErrorCode != "" || r.RecordName == "----Root-Folder----" || r.RecordName == "----Project-Root-Folder----" {
				continue
			}
			if n, ok := fieldNumber(r.Fields, "isDeleted"); ok && n == 1 {
				continue
			}
			raw := fieldString(r.Fields, "albumNameEnc")
			nameBytes, err := base64.StdEncoding.DecodeString(raw)
			name := string(nameBytes)
			if err != nil || strings.TrimSpace(name) == "" {
				name = "Album-" + r.RecordName
			}
			if t, ok := fieldNumber(r.Fields, "albumType"); ok && int(t) == folderAlbumType {
				if !visited[r.RecordName] { // a folder listed twice would be walked forever
					visited[r.RecordName] = true
					queue = append(queue, pending{r.RecordName, append(append([]string(nil), cur.names...), name)})
				}
				continue
			}
			albums = append(albums, PhotoAlbum{ID: r.RecordName, Names: append(append([]string(nil), cur.names...), name), Lib: lib})
		}
	}
	return albums, nil
}

func albumFilter(id string) []any {
	return []any{map[string]any{
		"fieldName": "parentId", "comparator": "EQUALS",
		"fieldValue": map[string]any{"type": "STRING", "value": id},
	}}
}

// AlbumList pages through the photos of one album.
func (c *Client) AlbumList(ctx context.Context, a PhotoAlbum, opts ListOptions, fn func([]PhotoAsset) error) error {
	if a.ID == "" {
		return errors.New("photos: album without an id")
	}
	return c.listAssets(ctx, a.Lib, "CPLContainerRelationLiveByAssetDate", albumFilter(a.ID), opts, fn)
}

// AlbumCount returns the number of photos Apple's index holds for the album.
func (c *Client) AlbumCount(ctx context.Context, a PhotoAlbum) (int, error) {
	return c.countObjType(ctx, a.Lib, "CPLContainerRelationNotDeletedByAssetDate:"+a.ID)
}
