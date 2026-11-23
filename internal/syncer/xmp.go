package syncer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/serebro/icloudsync/internal/icloud"
)

// buildXMP renders an XMP sidecar for a photo's metadata. The output depends
// only on the metadata (no clock, no local time zone, no build stamp), so an
// unchanged photo yields identical bytes and its sidecar is not rewritten.
func buildXMP(m *icloud.PhotoMeta) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="icloudsync">`)
	line(` <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">`)
	line(`  <rdf:Description rdf:about=""`)
	line(`    xmlns:dc="http://purl.org/dc/elements/1.1/"`)
	line(`    xmlns:xmp="http://ns.adobe.com/xap/1.0/"`)
	line(`    xmlns:exif="http://ns.adobe.com/exif/1.0/"`)
	line(`    xmlns:tiff="http://ns.adobe.com/tiff/1.0/"`)
	line(`    xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/"`)
	line(`    xmlns:Iptc4xmpExt="http://iptc.org/std/Iptc4xmpExt/2008-02-29/">`)

	if m.Caption != "" {
		line(`   <dc:title><rdf:Alt><rdf:li xml:lang="x-default">%s</rdf:li></rdf:Alt></dc:title>`, esc(m.Caption))
	}
	if m.Description != "" {
		line(`   <dc:description><rdf:Alt><rdf:li xml:lang="x-default">%s</rdf:li></rdf:Alt></dc:description>`, esc(m.Description))
	}
	if len(m.Keywords) > 0 {
		line(`   <dc:subject><rdf:Bag>`)
		for _, k := range m.Keywords {
			line(`    <rdf:li>%s</rdf:li>`, esc(k))
		}
		line(`   </rdf:Bag></dc:subject>`)
	}
	if !m.Created.IsZero() {
		stamp := m.Created.In(time.FixedZone("", m.TZOffset)).Format("2006-01-02T15:04:05-07:00")
		line(`   <xmp:CreateDate>%s</xmp:CreateDate>`, stamp)
		line(`   <photoshop:DateCreated>%s</photoshop:DateCreated>`, stamp)
	}
	switch {
	case m.Hidden:
		line(`   <xmp:Rating>-1</xmp:Rating>`) // -1 = rejected
	case m.Favorite:
		line(`   <xmp:Rating>5</xmp:Rating>`)
	}
	if m.HasGPS {
		line(`   <exif:GPSLatitude>%s</exif:GPSLatitude>`, gpsCoord(m.Lat, "N", "S"))
		line(`   <exif:GPSLongitude>%s</exif:GPSLongitude>`, gpsCoord(m.Lon, "E", "W"))
		if m.Alt != 0 {
			ref := 0
			if m.Alt < 0 {
				ref = 1
			}
			line(`   <exif:GPSAltitudeRef>%d</exif:GPSAltitudeRef>`, ref)
			line(`   <exif:GPSAltitude>%d/100</exif:GPSAltitude>`, int64(math.Round(math.Abs(m.Alt)*100)))
		}
		if !m.GPSTime.IsZero() && m.GPSTime.Year() > 1970 {
			line(`   <exif:GPSTimeStamp>%s</exif:GPSTimeStamp>`, m.GPSTime.UTC().Format("2006-01-02T15:04:05Z"))
		}
	}
	if m.Screenshot {
		line(`   <tiff:Make>Screenshot</tiff:Make>`)
		line(`   <Iptc4xmpExt:DigitalSourceType>screenCapture</Iptc4xmpExt:DigitalSourceType>`)
	}
	line(`  </rdf:Description>`)
	line(` </rdf:RDF>`)
	line(`</x:xmpmeta>`)
	return b.Bytes()
}

// gpsCoord formats a decimal degree as XMP's "DDD,MM.mmmmmmH".
func gpsCoord(v float64, pos, neg string) string {
	h := pos
	if v < 0 {
		h, v = neg, -v
	}
	deg := math.Floor(v)
	min := (v - deg) * 60
	// rounding the minutes up to 60 would produce an invalid value
	if s := fmt.Sprintf("%.6f", min); strings.HasPrefix(s, "60") {
		deg++
		min = 0
	}
	return fmt.Sprintf("%d,%.6f%s", int(deg), min, h)
}

func xmpVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return "xmp-" + hex.EncodeToString(sum[:8])
}
