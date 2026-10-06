package main

import (
	"net/url"
	"testing"
)

func BenchmarkRender60(b *testing.B) {
	loadFonts()
	d, _ := parseDesign(url.Values{"t": {"midnight"}, "end": {"2030-01-01"}})
	Render(d, 200000) // warm template
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Render(d, int64(200000-i%60))
	}
}
