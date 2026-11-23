package syncer

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/serebro/icloudsync/internal/icloud"
)

const (
	DriveRoot  = "Drive"
	PhotosRoot = "Photos"
)

func retry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	return icloud.Retry(ctx, nil, fn)
}

// DriveService lists iCloud Drive breadth-first.
func DriveService(c *icloud.Client) Service {
	return Service{Name: "drive", List: func(ctx context.Context, emit func(Item) error) error {
		type node struct{ id, rel string }
		queue := []node{{icloud.DriveRootID, DriveRoot}}
		for len(queue) > 0 {
			n := min(10, len(queue))
			batch := queue[:n]
			queue = queue[n:]
			ids := make([]string, n)
			for i, b := range batch {
				ids[i] = b.id
			}
			folders, err := retry(ctx, func() ([]icloud.DriveItem, error) { return c.DriveFolders(ctx, ids) })
			if err != nil {
				return err
			}
			for i, f := range folders {
				children := f.Items
				sort.SliceStable(children, func(a, b int) bool { return children[a].FileName() < children[b].FileName() })
				for _, ch := range children {
					rel := batch[i].rel + "/" + SanitizeName(ch.FileName())
					if ch.IsDir() {
						queue = append(queue, node{ch.DrivewsID, rel})
						continue
					}
					if ch.Type != "FILE" {
						continue
					}
					mt, _ := time.Parse(time.RFC3339, ch.DateModified)
					version := ch.Etag
					if version == "" {
						version = fmt.Sprintf("%d@%s", ch.Size, ch.DateModified)
					}
					item := ch
					err := emit(Item{
						Key: ch.DrivewsID, Desired: rel, Version: version, Size: ch.Size, ModTime: mt,
						Open: func(ctx context.Context) (io.ReadCloser, int64, error) {
							if item.Size == 0 {
								// Apple's content servers answer 400 for empty files
								return io.NopCloser(strings.NewReader("")), 0, nil
							}
							u, isPackage, err := c.DriveDownloadURL(ctx, item)
							if err != nil {
								return nil, 0, err
							}
							resp, err := c.Fetch(ctx, u, true)
							if err != nil {
								return nil, 0, err
							}
							if isPackage {
								return PackageBody{resp.Body}, -1, nil
							}
							return resp.Body, resp.ContentLength, nil
						},
					})
					if err != nil {
						return err
					}
				}
			}
		}
		return nil
	}}
}

// SharedAlbumsService mirrors iCloud Shared Albums (owned and subscribed) into
// Photos/SharedAlbums/<album name>/YYYY/MM/DD/ (by the photo's original date). Deletions happen only when every album
// listed completely and its item count matched what iCloud reports.
func SharedAlbumsService(c *icloud.Client, logf func(string, ...any)) Service {
	const name = "photos-albums"
	return Service{Name: name, List: func(ctx context.Context, emit func(Item) error) error {
		if _, err := c.ServiceURL("sharedstreams"); err != nil {
			logf("[%s] shared albums are not available for this account", name)
			return nil
		}
		albums, err := c.SharedAlbums(ctx)
		if err != nil {
			return err
		}
		if len(albums) == 0 {
			logf("[%s] no shared albums on this account", name)
			return nil
		}
		// a stable order keeps "Name" and "Name (2)" on the same albums across runs
		sort.SliceStable(albums, func(i, j int) bool {
			if albums[i].Created != albums[j].Created {
				return albums[i].Created < albums[j].Created
			}
			return albums[i].GUID < albums[j].GUID
		})
		used := map[string]bool{}
		var incomplete []string
		for _, a := range albums {
			base := SanitizeName(a.Name)
			if strings.TrimSpace(base) == "" {
				base = "Album"
			}
			dir := base
			for n := 2; used[foldKey(dir)]; n++ {
				dir = fmt.Sprintf("%s (%d)", base, n)
			}
			used[foldKey(dir)] = true
			root := PhotosRoot + "/SharedAlbums/" + dir
			ids := map[string]bool{}
			err := c.SharedAlbumList(ctx, a, func(page []icloud.PhotoAsset) error {
				for _, p := range page {
					if ids[p.ID] {
						continue
					}
					ids[p.ID] = true
					for _, it := range photoItemsIn(c, datedDir(root, p), a.GUID+"/", p, PhotoOptions{}) {
						if err := emit(it); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				if ctx.Err() != nil || icloud.IsAuthError(err) {
					return err
				}
				// keep going with the other albums; this run just won't delete
				logf("[%s] album %q: %v", name, a.Name, err)
				incomplete = append(incomplete, a.Name)
				continue
			}
			want, err := c.SharedAlbumCount(ctx, a)
			switch {
			case err != nil:
				logf("[%s] album %q: item count unavailable: %v", name, a.Name, err)
				incomplete = append(incomplete, a.Name)
			case want != len(ids):
				logf("[%s] album %q: listed %d items but iCloud counts %d", name, a.Name, len(ids), want)
				incomplete = append(incomplete, a.Name)
			}
		}
		if len(incomplete) > 0 {
			return fmt.Errorf("%d of %d shared albums listed incompletely (%s)", len(incomplete), len(albums), strings.Join(incomplete, ", "))
		}
		return nil
	}}
}
