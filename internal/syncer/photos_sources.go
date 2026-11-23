package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/serebro/icloudsync/internal/icloud"
)

// RecentDirection is the CloudKit direction that lists the newest photos first;
// --recent and --until-found walk the library in this order.
var RecentDirection = "ASCENDING"

// PhotoOptions selects what is mirrored from the photo libraries. The zero
// value is the plain mirror: original files only, whole library.
type PhotoOptions struct {
	// Sizes lists the renditions to download, first one first. Empty means
	// the original only. Videos always come as the original.
	Sizes []icloud.PhotoSize
	// ForceSize skips a photo that lacks a requested rendition instead of
	// falling back to its original.
	ForceSize bool
	// XMP writes an .xmp sidecar (caption, keywords, location, favorite) next
	// to each photo that has such metadata.
	XMP bool
	// Albums names personal albums to mirror into Photos/Albums/ (a path like
	// "Folder/Album" or the album's own name); AllAlbums mirrors every album.
	Albums    []string
	AllAlbums bool
	// Recent limits the listing to the first N photos, UntilFound stops after N
	// consecutive photos that are already up to date. Both make the listing
	// partial on purpose, so nothing is deleted.
	Recent     int
	UntilFound int
}

func (o PhotoOptions) sizes() []icloud.PhotoSize {
	if len(o.Sizes) == 0 {
		return []icloud.PhotoSize{icloud.SizeOriginal}
	}
	return o.Sizes
}

// wantsVersions: does any requested size need the extra renditions?
func (o PhotoOptions) wantsVersions() bool {
	for _, s := range o.sizes() {
		if s != icloud.SizeOriginal {
			return true
		}
	}
	return false
}

func (o PhotoOptions) partial() bool { return o.Recent > 0 || o.UntilFound > 0 }

func (o PhotoOptions) listOptions() icloud.ListOptions {
	lo := icloud.ListOptions{Sizes: o.wantsVersions(), Meta: o.XMP}
	if o.partial() {
		lo.Direction = RecentDirection
	}
	return lo
}

func (o PhotoOptions) wantsAlbums() bool { return o.AllAlbums || len(o.Albums) > 0 }

// PhotosServices is PhotosServicesWith for a plain mirror.
func PhotosServices(c *icloud.Client, logf func(string, ...any)) []Service {
	return PhotosServicesWith(c, logf, PhotoOptions{})
}

// PhotosServicesWith returns one service per library kind: the personal library
// into Photos/Primary/YYYY/MM/DD/, the iCloud Shared Photo Library into
// Photos/Shared/YYYY/MM/DD/, the Shared Albums, and (when asked for) the
// personal albums into Photos/Albums/<album>/YYYY/MM/DD/. Each has its own
// manifest section, so a problem with one never causes deletions in another.
func PhotosServicesWith(c *icloud.Client, logf func(string, ...any), po PhotoOptions) []Service {
	svcs := []Service{
		photosService(c, logf, po, "photos", "primary", PhotosRoot+"/Primary"),
		photosService(c, logf, po, "photos-shared", "shared", PhotosRoot+"/Shared"),
		SharedAlbumsService(c, logf),
	}
	if po.wantsAlbums() {
		svcs = append(svcs, PersonalAlbumsService(c, logf, po))
	}
	return svcs
}

func withPartial(svc Service, po PhotoOptions) Service {
	svc.Partial = po.partial()
	svc.UntilFound = po.UntilFound
	return svc
}

// errAlbumLimit ends one album's listing when --recent is reached; the next
// album is still listed.
var errAlbumLimit = errors.New("album listing limit reached")

// recentLimiter counts assets and says when --recent is satisfied.
type recentLimiter struct {
	max, n int
}

// done is called after an asset was emitted.
func (r *recentLimiter) done() bool {
	if r.max <= 0 {
		return false
	}
	r.n++
	return r.n >= r.max
}

func photosService(c *icloud.Client, logf func(string, ...any), po PhotoOptions, name, kind, root string) Service {
	return withPartial(Service{Name: name, List: func(ctx context.Context, emit func(Item) error) error {
		all, err := retry(ctx, func() ([]icloud.PhotoLibrary, error) { return c.PhotoLibraries(ctx) })
		if err != nil {
			return err
		}
		var libs []icloud.PhotoLibrary
		for _, l := range all {
			if l.Kind == kind {
				libs = append(libs, l)
			}
		}
		if len(libs) == 0 {
			logf("[%s] no %s library on this account", name, kind)
			return nil
		}
		lim := &recentLimiter{max: po.Recent}
		for _, lib := range libs {
			if err := listLibrary(ctx, c, logf, name, root, lib, po, lim, emit); err != nil {
				return err
			}
		}
		return nil
	}}, po)
}

// checkIndexed waits for Apple's index and reports whether it finished. Apple
// often reports RUNNING/FAILED for libraries that list fine (icloudpd #1387),
// so a library that is not indexed is still listed; callers then trust the
// listing only when the item count matches.
func checkIndexed(ctx context.Context, c *icloud.Client, logf func(string, ...any), name string, lib icloud.PhotoLibrary) (bool, error) {
	_, err := retry(ctx, func() (struct{}, error) { return struct{}{}, c.PhotosReady(ctx, lib) })
	var notReady *icloud.PhotosNotReadyError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &notReady):
		logf("[%s] %v; continuing, deletions only if the item count matches", name, err)
		return false, nil
	default:
		return false, err
	}
}

func listLibrary(ctx context.Context, c *icloud.Client, logf func(string, ...any), name, root string, lib icloud.PhotoLibrary, po PhotoOptions, lim *recentLimiter, emit func(Item) error) error {
	indexed, err := checkIndexed(ctx, c, logf, name, lib)
	if err != nil {
		return err
	}
	// count unique assets: a page boundary that repeats one asset must not
	// hide another that was skipped
	ids := map[string]bool{}
	if err := c.PhotosListWith(ctx, lib, po.listOptions(), func(page []icloud.PhotoAsset) error {
		for _, a := range page {
			if ids[a.ID] {
				continue
			}
			ids[a.ID] = true
			for _, it := range photoItems(c, root, a, po) {
				if err := emit(it); err != nil {
					return err
				}
			}
			if lim.done() {
				return ErrStopListing
			}
		}
		return nil
	}); err != nil {
		return err // ErrStopListing passes through: a partial listing has no count to check
	}
	return verifyCount(ctx, logf, name, lib.ZoneName(), indexed, len(ids), func() (int, error) { return c.PhotosCount(ctx, lib) })
}

// verifyCount compares what was listed with Apple's own item count.
func verifyCount(ctx context.Context, logf func(string, ...any), name, what string, indexed bool, listed int, count func() (int, error)) error {
	want, cerr := retry(ctx, count)
	switch {
	case cerr != nil && indexed:
		logf("[%s] could not read item count (%v); trusting the indexed listing", name, cerr)
	case cerr != nil:
		return fmt.Errorf("%s: listing not verifiable (library not indexed, count unavailable: %v)", what, cerr)
	case listed != want:
		return fmt.Errorf("%s: listed %d items but iCloud counts %d; treating listing as incomplete", what, listed, want)
	}
	return nil
}

// PersonalAlbumsService mirrors albums of the personal library into
// Photos/Albums/<folder>/<album>/YYYY/MM/DD/. Photos that are in the library
// and in an album are stored twice (once per tree). Deletions happen only when
// every album listed completely and its item count matched what iCloud reports.
func PersonalAlbumsService(c *icloud.Client, logf func(string, ...any), po PhotoOptions) Service {
	const name = "photos-myalbums"
	return withPartial(Service{Name: name, List: func(ctx context.Context, emit func(Item) error) error {
		all, err := retry(ctx, func() ([]icloud.PhotoLibrary, error) { return c.PhotoLibraries(ctx) })
		if err != nil {
			return err
		}
		var lib *icloud.PhotoLibrary
		for i := range all {
			if all[i].Kind == "primary" {
				lib = &all[i]
				break
			}
		}
		if lib == nil {
			logf("[%s] no personal library on this account", name)
			return nil
		}
		indexed, err := checkIndexed(ctx, c, logf, name, *lib)
		if err != nil {
			return err
		}
		albums, err := retry(ctx, func() ([]icloud.PhotoAlbum, error) { return c.PhotoAlbums(ctx, *lib) })
		if err != nil {
			return err
		}
		chosen, err := chooseAlbums(albums, po)
		if err != nil {
			return err
		}
		if len(chosen) == 0 {
			logf("[%s] no albums on this account", name)
			return nil
		}
		used := map[string]bool{}
		var incomplete []string
		for _, a := range chosen {
			root := PhotosRoot + "/Albums/" + albumDir(a, used)
			ids := map[string]bool{}
			lim := &recentLimiter{max: po.Recent} // --recent counts per album
			err := c.AlbumList(ctx, a, po.listOptions(), func(page []icloud.PhotoAsset) error {
				for _, p := range page {
					if ids[p.ID] {
						continue
					}
					ids[p.ID] = true
					for _, it := range photoItemsIn(c, datedDir(root, p), a.ID+"/", p, po) {
						if err := emit(it); err != nil {
							return err
						}
					}
					if lim.done() {
						return errAlbumLimit
					}
				}
				return nil
			})
			if errors.Is(err, errAlbumLimit) {
				continue // this album is cut short on purpose: no count to check, the service is partial
			}
			if err != nil {
				if errors.Is(err, ErrStopListing) || ctx.Err() != nil || icloud.IsAuthError(err) {
					return err
				}
				logf("[%s] album %q: %v", name, a.Path(), err)
				incomplete = append(incomplete, a.Path())
				continue
			}
			if verr := verifyCount(ctx, logf, name, fmt.Sprintf("album %q", a.Path()), indexed, len(ids), func() (int, error) { return c.AlbumCount(ctx, a) }); verr != nil {
				logf("[%s] %v", name, verr)
				incomplete = append(incomplete, a.Path())
			}
		}
		if len(incomplete) > 0 {
			return fmt.Errorf("%d of %d albums listed incompletely (%s)", len(incomplete), len(chosen), strings.Join(incomplete, ", "))
		}
		return nil
	}}, po)
}

// chooseAlbums picks the albums named with --album (by "Folder/Album" path or
// by the album's own name), or all of them with --albums.
func chooseAlbums(albums []icloud.PhotoAlbum, po PhotoOptions) ([]icloud.PhotoAlbum, error) {
	sort.SliceStable(albums, func(i, j int) bool { return albums[i].Path() < albums[j].Path() })
	if po.AllAlbums {
		return albums, nil
	}
	var out []icloud.PhotoAlbum
	picked := map[string]bool{}
	for _, want := range po.Albums {
		found := false
		for _, a := range albums {
			if a.Path() == want || a.Name() == want {
				found = true
				if !picked[a.ID] {
					picked[a.ID] = true
					out = append(out, a)
				}
			}
		}
		if !found {
			names := make([]string, len(albums))
			for i, a := range albums {
				names[i] = a.Path()
			}
			return nil, fmt.Errorf("album %q not found (albums: %s)", want, strings.Join(names, ", "))
		}
	}
	return out, nil
}

// albumDir is the folder name of an album: sanitized components, with a
// numeric suffix when another album already took the name (case-insensitively).
func albumDir(a icloud.PhotoAlbum, used map[string]bool) string {
	parts := make([]string, len(a.Names))
	for i, n := range a.Names {
		parts[i] = SanitizeName(n)
	}
	base := strings.Join(parts, "/")
	dir := base
	for n := 2; used[foldKey(dir)]; n++ {
		last := len(parts) - 1
		parts[last] = fmt.Sprintf("%s (%d)", SanitizeName(a.Names[last]), n)
		dir = strings.Join(parts, "/")
	}
	used[foldKey(dir)] = true
	return dir
}

// photoItems places an asset into root/YYYY/MM/DD/.
func photoItems(c *icloud.Client, root string, a icloud.PhotoAsset, po PhotoOptions) []Item {
	return photoItemsIn(c, datedDir(root, a), "", a, po)
}

func datedDir(root string, a icloud.PhotoAsset) string {
	if a.Date.IsZero() {
		return root + "/unknown-date"
	}
	return root + "/" + a.Date.Local().Format("2006/01/02")
}

func openResource(c *icloud.Client, u string) func(ctx context.Context) (io.ReadCloser, int64, error) {
	return func(ctx context.Context) (io.ReadCloser, int64, error) {
		resp, err := c.Fetch(ctx, u, false)
		if err != nil {
			return nil, 0, err
		}
		return resp.Body, resp.ContentLength, nil
	}
}

// baseName is the file name an original gets.
func baseName(a icloud.PhotoAsset) string {
	name := SanitizeName(a.Filename)
	if strings.TrimSpace(a.Filename) == "" {
		id := SanitizeName(a.ID)
		if len(id) > 12 {
			id = id[:12]
		}
		name = id + "." + icloud.ItemTypeExt(a.ItemType)
	}
	return name
}

// renditionName is the file name of a non-original rendition: medium and thumb
// always carry a suffix, adjusted and alternative only when the original is
// downloaded too (the same rule icloudpd uses). The extension follows the
// rendition's own type, as an edited HEIC photo is rendered as a JPEG.
func renditionName(name string, size icloud.PhotoSize, res *icloud.PhotoResource, withOriginal bool) string {
	stem := strings.TrimSuffix(name, path.Ext(name))
	ext := path.Ext(name)
	if e := icloud.ItemTypeExt(res.FileType); e != "bin" {
		ext = "." + e
	}
	suffix := ""
	switch size {
	case icloud.SizeMedium, icloud.SizeThumb:
		suffix = "-" + string(size)
	case icloud.SizeAdjusted, icloud.SizeAlternative:
		if withOriginal {
			suffix = "-" + string(size)
		}
	}
	return stem + suffix + ext
}

// photoItemsIn returns the files of one asset placed directly in dir; manifest
// keys are the asset ID with keyPrefix prepended. The first file keeps the plain
// asset ID as its key whichever rendition it is, so changing --size replaces it
// in place; further files and the Live video and sidecar get suffixed keys.
func photoItemsIn(c *icloud.Client, dir, keyPrefix string, a icloud.PhotoAsset, po PhotoOptions) []Item {
	name := baseName(a)
	mk := func(key, rel string, r *icloud.PhotoResource) Item {
		version := r.Checksum
		if version == "" {
			version = fmt.Sprintf("%d", r.Size)
		}
		return Item{
			Key: key, Group: keyPrefix + a.ID, Desired: rel, Version: version, Size: r.Size, ModTime: a.Date,
			Open: openResource(c, r.URL),
		}
	}

	sizes := po.sizes()
	if icloud.IsMovieType(a.ItemType) {
		sizes = []icloud.PhotoSize{icloud.SizeOriginal}
	}
	withOriginal := false
	fullSize := false // does the Live video belong with the chosen renditions?
	for _, s := range sizes {
		if s == icloud.SizeOriginal {
			withOriginal = true
		}
		if s == icloud.SizeOriginal || s == icloud.SizeAdjusted || s == icloud.SizeAlternative {
			fullSize = true
		}
	}

	var items []Item
	done := map[*icloud.PhotoResource]bool{}
	for _, size := range sizes {
		res, fell := a.Original, false
		if size != icloud.SizeOriginal {
			res = a.Versions[size]
			if res == nil {
				if po.ForceSize {
					continue
				}
				res, fell = a.Original, true // the photo has no such rendition
			}
		}
		if done[res] {
			continue
		}
		done[res] = true
		fname, suffix := name, ""
		switch {
		case fell || size == icloud.SizeOriginal:
			if len(items) > 0 {
				suffix = "#original"
			}
		default:
			fname = renditionName(name, size, res, withOriginal)
			if len(items) > 0 {
				suffix = "#" + string(size)
			}
		}
		items = append(items, mk(keyPrefix+a.ID+suffix, dir+"/"+fname, res))
	}
	if len(items) == 0 {
		return nil
	}
	if a.Live != nil && fullSize {
		stem := strings.TrimSuffix(name, path.Ext(name))
		items = append(items, mk(keyPrefix+a.ID+"#live", dir+"/"+stem+"_HEVC.MOV", a.Live))
	}
	if po.XMP && !a.Meta.Empty() {
		data := buildXMP(a.Meta)
		items = append(items, Item{
			Key: keyPrefix + a.ID + "#xmp", Group: keyPrefix + a.ID, Desired: items[0].Desired + ".xmp",
			Version: xmpVersion(data), Size: int64(len(data)), ModTime: a.Date,
			Open: func(context.Context) (io.ReadCloser, int64, error) {
				return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
			},
		})
	}
	return items
}
