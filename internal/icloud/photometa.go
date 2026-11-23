package icloud

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"howett.net/plist"
)

// PhotoMeta is the descriptive metadata iCloud keeps about a photo.
type PhotoMeta struct {
	Caption     string
	Description string
	Keywords    []string
	HasGPS      bool
	Lat, Lon    float64
	Alt, Speed  float64
	GPSTime     time.Time
	Favorite    bool
	Hidden      bool
	Screenshot  bool
	Created     time.Time // assetDate
	TZOffset    int       // seconds east of UTC, as recorded with the photo
}

// Empty reports whether there is nothing worth writing to a sidecar. The
// creation date alone does not count: the file already carries it.
func (m *PhotoMeta) Empty() bool {
	return m == nil || (m.Caption == "" && m.Description == "" && len(m.Keywords) == 0 &&
		!m.HasGPS && !m.Favorite && !m.Hidden && !m.Screenshot)
}

func fieldNumber(fields map[string]ckField, key string) (float64, bool) {
	f, ok := fields[key]
	if !ok || len(f.Value) == 0 {
		return 0, false
	}
	var n float64
	if json.Unmarshal(f.Value, &n) != nil {
		return 0, false
	}
	return n, true
}

// fieldBytes returns the base64-decoded payload of an ENCRYPTED_BYTES / BYTES field.
func fieldBytes(fields map[string]ckField, key string) []byte {
	s := fieldString(fields, key)
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// buildMeta reads the metadata fields of an asset record. Fields iCloud sends
// in an unexpected format are left out rather than failing the photo.
func buildMeta(a ckRecord) *PhotoMeta {
	f := a.Fields
	m := &PhotoMeta{}
	m.Caption = string(fieldBytes(f, "captionEnc"))
	m.Description = string(fieldBytes(f, "extendedDescEnc"))
	if raw := fieldBytes(f, "keywordsEnc"); len(raw) > 0 {
		var kw []string
		if _, err := plist.Unmarshal(raw, &kw); err == nil {
			m.Keywords = kw
		}
	}
	if raw := fieldBytes(f, "locationEnc"); len(raw) > 0 {
		var loc struct {
			Lat   float64   `plist:"lat"`
			Lon   float64   `plist:"lon"`
			Alt   float64   `plist:"alt"`
			Speed float64   `plist:"speed"`
			Time  time.Time `plist:"timestamp"`
		}
		if _, err := plist.Unmarshal(raw, &loc); err == nil && (loc.Lat != 0 || loc.Lon != 0) {
			m.HasGPS = true
			m.Lat, m.Lon, m.Alt, m.Speed, m.GPSTime = loc.Lat, loc.Lon, loc.Alt, loc.Speed, loc.Time
		}
	}
	if n, ok := fieldNumber(f, "isFavorite"); ok {
		m.Favorite = n == 1
	}
	if n, ok := fieldNumber(f, "isHidden"); ok {
		m.Hidden = n == 1
	}
	if n, ok := fieldNumber(f, "assetSubtypeV2"); ok {
		m.Screenshot = int(n) == 3
	}
	if n, ok := fieldNumber(f, "timeZoneOffset"); ok {
		m.TZOffset = int(n)
	}
	if n, ok := fieldNumber(f, "assetDate"); ok && n != 0 {
		m.Created = time.UnixMilli(int64(n))
	}
	return m
}
