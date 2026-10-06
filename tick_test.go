package main

import (
	"bytes"
	"image/gif"
	"net/url"
	"testing"
	"time"
)

func TestUnitValues(t *testing.T) {
	r := int64(2*86400 + 7*3600 + 13*60 + 42)
	if got := unitValues("dhms", r); got[0] != 2 || got[1] != 7 || got[2] != 13 || got[3] != 42 {
		t.Fatalf("dhms: %v", got)
	}
	if got := unitValues("hms", r); got[0] != 55 { // days roll into hours
		t.Fatalf("hms: %v", got)
	}
}

func TestSignatureIgnoresFromButCoversDesign(t *testing.T) {
	secret := []byte("s")
	q := url.Values{"dur": {"48h"}, "fg": {"ffffff"}}
	q.Set("sig", sign(secret, q))
	q.Set("from", "1700000000") // substituted by the ESP after signing
	if !verify(secret, q) {
		t.Fatal("from must not affect the signature")
	}
	q.Set("fg", "ff0000")
	if verify(secret, q) {
		t.Fatal("design change must invalidate the signature")
	}
}

func TestRenderFramesAndExpiry(t *testing.T) {
	if err := loadFonts(); err != nil {
		t.Fatal(err)
	}
	d, err := parseDesign(url.Values{"end": {time.Now().Add(time.Hour).Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Render(d, 3600)
	g, err := gif.DecodeAll(bytes.NewReader(b))
	if err != nil || len(g.Image) != 60 {
		t.Fatalf("want 60 frames, got %d (%v)", len(g.Image), err)
	}
	if g.Image[0].Bounds() != g.Image[0].Rect || g.Image[0].Rect.Dx() != g.Config.Width {
		t.Fatal("first frame must be full-size (Outlook shows only frame 1)")
	}
	b, _ = Render(d, 5)
	g, _ = gif.DecodeAll(bytes.NewReader(b))
	if len(g.Image) != 6 { // 5,4,3,2,1,0 then hold
		t.Fatalf("near zero: want 6 frames, got %d", len(g.Image))
	}
	d.Expired = "Ended"
	b, _ = Render(d, 0)
	g, _ = gif.DecodeAll(bytes.NewReader(b))
	if len(g.Image) != 1 {
		t.Fatal("expired should be a single frame")
	}
}
