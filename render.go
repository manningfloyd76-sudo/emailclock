package main

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"math"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Rendering strategy
//
// A countdown GIF is ~60 frames that differ only in a few digit cells. So:
//
//  1. Per design we build a Template once: the static base image (background,
//     boxes, labels, separators) already quantized to a fixed palette, plus a
//     pre-rasterized alpha mask for each digit 0-9 and a 256-entry lookup that
//     maps mask coverage straight to a palette index. Templates are timing-
//     independent, so every campaign using the same look shares one.
//  2. Per request we only stamp digit masks into cells — no font
//     rasterization, no color quantization, no per-frame full-image work.
//  3. Frames after the first are emitted as sub-rectangles covering only the
//     cells that changed (usually one 2-digit cell), which keeps GIFs tiny.
//
// Frame 1 is always a complete, correct image: Outlook for Windows shows
// only the first frame, so it must stand on its own.

//go:embed fonts/*
var fontFS embed.FS

var fonts = map[string]*opentype.Font{}

func loadFonts() error {
	for name, file := range map[string]string{
		"inter": "fonts/inter.otf", "label": "fonts/label.otf", "poppins": "fonts/poppins.ttf",
		"serif": "fonts/serif.ttf", "mono": "fonts/mono.ttf",
	} {
		b, err := fontFS.ReadFile(file)
		if err != nil {
			return err
		}
		f, err := opentype.Parse(b)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		fonts[name] = f
	}
	return nil
}

func face(name string, px float64) font.Face {
	f, _ := opentype.NewFace(fonts[name], &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingNone})
	return f
}

// --- template ----------------------------------------------------------------

type cell struct{ r image.Rectangle } // where one digit lives, in pixels

type template struct {
	w, h    int
	palette color.Palette
	base    *image.Paletted
	masks   [10]*image.Alpha // each cellW x cellH
	lut     [256]uint8       // mask coverage -> palette index (digit over box)
	cells   [][]cell         // [unit][digit]
	ink     image.Rectangle  // union of all digit glyph bounds, relative to a cell
	expired bool
}

var (
	tplMu    sync.Mutex
	tplCache = map[string]*template{}
)

// lookKey identifies everything that affects pixels except the numbers.
func lookKey(d *Design, digits []int) string {
	return fmt.Sprintf("%s|%s|%q|%v|%v|%.3f|%v|%v%v%v%v%v|%d|%d|%v|%q",
		d.Font, d.Units, d.Labels, d.Upper, d.Boxes, d.Radius, d.Sep,
		d.BG, d.Box, d.FG, d.Label, d.SepColor, d.Width, d.Scale, digits, d.Expired)
}

func getTemplate(d *Design, digits []int, expired bool) *template {
	key := lookKey(d, digits)
	if expired {
		key = "EXP|" + key
	}
	tplMu.Lock()
	defer tplMu.Unlock()
	if t, ok := tplCache[key]; ok {
		return t
	}
	var t *template
	if expired {
		t = buildExpired(d)
	} else {
		t = buildTemplate(d, digits)
	}
	if len(tplCache) > 2000 { // crude bound; production would use an LRU
		tplCache = map[string]*template{}
	}
	tplCache[key] = t
	return t
}

// geometry is shared by the countdown and expired templates so both are the
// same size and can be swapped in an email without layout shift.
type geometry struct {
	W, H, pad, gap, unitW, boxY, boxH int
	digitPx, labelPx, cellW           float64
	labelY                            int
}

func measure(d *Design, digits []int) geometry {
	s := float64(d.Scale)
	W := d.Width * d.Scale
	n := len(d.Units)
	g := geometry{W: W}
	g.pad = int(math.Round(0.04 * float64(W)))
	gapFrac := 0.028
	if d.Sep {
		gapFrac = 0.055
	}
	g.gap = int(math.Round(gapFrac * float64(W)))
	g.unitW = (W - 2*g.pad - (n-1)*g.gap) / n

	maxDigits := 2
	for _, k := range digits {
		maxDigits = max(maxDigits, k)
	}
	// Tabular digit cell width at 100px, then scale to fit the unit.
	f100 := face(d.Font, 100)
	adv100 := 0.0
	for c := '0'; c <= '9'; c++ {
		a, _ := f100.GlyphAdvance(c)
		adv100 = max(adv100, float64(a)/64)
	}
	fillW := 0.68
	if !d.Boxes {
		fillW = 0.92
	}
	g.digitPx = math.Min(float64(g.unitW)*fillW/(float64(maxDigits)*adv100/100), float64(g.unitW)*0.54)
	g.cellW = math.Ceil(adv100 * g.digitPx / 100)

	if d.Boxes {
		g.boxH = int(math.Round(g.digitPx * 1.32))
	} else {
		g.boxH = int(math.Round(g.digitPx * 1.04))
	}
	g.boxY = g.pad

	// Labels: one size for all units, shrunk until the widest fits.
	g.labelPx = math.Max(g.digitPx*0.2, 10*s)
	for g.labelPx > 6*s {
		lf := face("label", g.labelPx)
		widest := 0.0
		for _, l := range d.Labels {
			widest = math.Max(widest, float64(font.MeasureString(lf, l))/64+tracking(g.labelPx, d.Upper)*float64(len([]rune(l))))
		}
		if widest <= float64(g.unitW)*0.96 {
			break
		}
		g.labelPx *= 0.94
	}
	labelGap := int(math.Round(math.Max(g.digitPx*0.16, 6*s)))
	g.labelY = g.boxY + g.boxH + labelGap
	g.H = g.labelY + int(math.Round(g.labelPx*1.2)) + int(math.Round(float64(g.pad)*0.85))
	return g
}

func tracking(px float64, upper bool) float64 {
	if upper {
		return px * 0.12 // open letterspacing reads as premium on small caps labels
	}
	return 0
}

func buildTemplate(d *Design, digits []int) *template {
	g := measure(d, digits)
	t := &template{w: g.W, h: g.H}

	// 1) Base image in true color (antialiased shapes and text).
	rgba := image.NewRGBA(image.Rect(0, 0, g.W, g.H))
	draw.Draw(rgba, rgba.Bounds(), image.NewUniform(d.BG), image.Point{}, draw.Src)

	digitFace := face(d.Font, g.digitPx)
	zb, _ := font.BoundString(digitFace, "0")
	digitMid := float64(zb.Min.Y+zb.Max.Y) / 128 // vertical center of digits relative to baseline
	baseline := float64(g.boxH)/2 - digitMid

	labelFace := face("label", g.labelPx)
	labelAscent := float64(labelFace.Metrics().Ascent) / 64

	t.cells = make([][]cell, len(d.Units))
	for i := range d.Units {
		ux := g.pad + i*(g.unitW+g.gap)
		if d.Boxes {
			roundRect(rgba, float64(ux), float64(g.boxY), float64(g.unitW), float64(g.boxH), d.Radius*float64(g.boxH), d.Box)
		}
		// digit cells, centered in the unit
		nd := digits[i]
		cw := int(g.cellW)
		x0 := ux + (g.unitW-nd*cw)/2
		for k := 0; k < nd; k++ {
			t.cells[i] = append(t.cells[i], cell{image.Rect(x0+k*cw, g.boxY, x0+(k+1)*cw, g.boxY+g.boxH)})
		}
		// label
		lbl := d.Labels[i]
		tr := tracking(g.labelPx, d.Upper)
		lw := float64(font.MeasureString(labelFace, lbl))/64 + tr*float64(len([]rune(lbl))-1)
		x := float64(ux) + (float64(g.unitW)-lw)/2
		drawText(rgba, labelFace, lbl, x, float64(g.labelY)+labelAscent, tr, d.Label)
		// separator in the gap after this unit
		if d.Sep && i < len(d.Units)-1 {
			sf := face(d.Font, g.digitPx*0.8)
			cb, _ := font.BoundString(sf, ":")
			cw := float64(cb.Max.X-cb.Min.X) / 64
			cx := float64(ux+g.unitW) + float64(g.gap)/2 - cw/2 - float64(cb.Min.X)/64
			cy := float64(g.boxY) + float64(g.boxH)/2 - float64(cb.Min.Y+cb.Max.Y)/128
			drawText(rgba, sf, ":", cx, cy, 0, d.SepColor)
		}
	}

	// 2) Fixed palette built from the design's own color ramps. Every pixel in
	// the base is an antialiased blend of exactly two of these colors, so
	// quantization is near-lossless and needs no dithering.
	t.palette = buildPalette(d.Box, [][2]color.RGBA{
		{d.BG, d.Box}, {d.Box, d.FG}, {d.BG, d.FG}, {d.BG, d.Label}, {d.BG, d.SepColor},
	})
	t.base = quantize(rgba, t.palette)

	// 3) Digit masks + coverage lookup.
	for c := 0; c < 10; c++ {
		m := image.NewAlpha(image.Rect(0, 0, int(g.cellW), g.boxH))
		adv, _ := digitFace.GlyphAdvance(rune('0' + c))
		x := (g.cellW - float64(adv)/64) / 2
		dr := font.Drawer{Dst: m, Src: image.Opaque, Face: digitFace, Dot: fixed.Point26_6{X: fx(x), Y: fx(baseline)}}
		dr.DrawString(string(rune('0' + c)))
		t.masks[c] = m
		t.ink = t.ink.Union(inkBounds(m))
	}
	for a := 0; a < 256; a++ {
		t.lut[a] = uint8(t.palette.Index(lerp(d.Box, d.FG, float64(a)/255)))
	}
	return t
}

func buildExpired(d *Design) *template {
	digits := make([]int, len(d.Units))
	for i := range digits {
		digits[i] = 2
	}
	g := measure(d, digits) // same footprint as the live timer
	rgba := image.NewRGBA(image.Rect(0, 0, g.W, g.H))
	draw.Draw(rgba, rgba.Bounds(), image.NewUniform(d.BG), image.Point{}, draw.Src)
	px := float64(g.H) * 0.34
	var f font.Face
	for ; px > 8; px *= 0.95 {
		f = face(d.Font, px)
		if float64(font.MeasureString(f, d.Expired))/64 <= float64(g.W)*0.88 {
			break
		}
	}
	b, adv := font.BoundString(f, d.Expired)
	x := (float64(g.W) - float64(adv)/64) / 2
	y := float64(g.H)/2 - float64(b.Min.Y+b.Max.Y)/128
	drawText(rgba, f, d.Expired, x, y, 0, d.FG)
	pal := buildPalette(d.BG, [][2]color.RGBA{{d.BG, d.FG}})
	return &template{w: g.W, h: g.H, palette: pal, base: quantize(rgba, pal), expired: true}
}

// --- frames ------------------------------------------------------------------

// unitValues splits remaining seconds across the chosen units. The largest
// chosen unit absorbs everything above it (e.g. "hms" shows 49 hours).
func unitValues(units string, rem int64) []int64 {
	size := map[byte]int64{'d': 86400, 'h': 3600, 'm': 60, 's': 1}
	out := make([]int64, len(units))
	for i := 0; i < len(units); i++ {
		out[i] = rem / size[units[i]]
		rem -= out[i] * size[units[i]]
	}
	return out
}

func digitCounts(vals []int64) []int {
	out := make([]int, len(vals))
	for i, v := range vals {
		out[i] = max(2, min(4, len(fmt.Sprint(v))))
	}
	return out
}

// Render produces the GIF for a timer with `remaining` whole seconds left.
func Render(d *Design, remaining int64) ([]byte, error) {
	if remaining <= 0 && d.Expired != "" {
		t := getTemplate(d, nil, true)
		return encode(t, []*image.Paletted{t.base}, d.Loop)
	}
	remaining = max(remaining, 0)
	t := getTemplate(d, digitCounts(unitValues(d.Units, remaining)), false)

	n := int64(d.Frames)
	if d.Units[len(d.Units)-1] != 's' {
		n = 1 // nothing changes within a minute; a static image is smaller
	}
	n = min(n, remaining+1)

	cur := image.NewPaletted(t.base.Rect, t.palette)
	copy(cur.Pix, t.base.Pix)
	var prev [][]int
	frames := make([]*image.Paletted, 0, n)

	for i := int64(0); i < n; i++ {
		vals := unitValues(d.Units, remaining-i)
		dig := make([][]int, len(vals))
		var dirty image.Rectangle
		for u, v := range vals {
			nd := len(t.cells[u])
			dig[u] = make([]int, nd)
			for k := nd - 1; k >= 0; k-- {
				dig[u][k] = int(v % 10)
				v /= 10
			}
			for k := 0; k < nd; k++ {
				if prev != nil && prev[u][k] == dig[u][k] {
					continue
				}
				r := t.cells[u][k].r
				stamp(cur, t, r, dig[u][k])
				dirty = dirty.Union(t.ink.Add(r.Min))
			}
		}
		prev = dig
		if i == 0 {
			dirty = cur.Rect
		}
		fr := image.NewPaletted(dirty, t.palette)
		for y := dirty.Min.Y; y < dirty.Max.Y; y++ {
			copy(fr.Pix[fr.PixOffset(dirty.Min.X, y):fr.PixOffset(dirty.Max.X, y)], cur.Pix[cur.PixOffset(dirty.Min.X, y):cur.PixOffset(dirty.Max.X, y)])
		}
		frames = append(frames, fr)
	}
	return encode(t, frames, d.Loop)
}

// stamp restores a cell from the base and draws digit c into it.
func stamp(dst *image.Paletted, t *template, r image.Rectangle, c int) {
	m := t.masks[c]
	for y := 0; y < r.Dy(); y++ {
		row := dst.PixOffset(r.Min.X, r.Min.Y+y)
		copy(dst.Pix[row:row+r.Dx()], t.base.Pix[row:row+r.Dx()])
		mrow := m.Pix[y*m.Stride : y*m.Stride+r.Dx()]
		for x, a := range mrow {
			if a > 0 {
				dst.Pix[row+x] = t.lut[a]
			}
		}
	}
}

func encode(t *template, frames []*image.Paletted, loop bool) ([]byte, error) {
	g := &gif.GIF{
		Image:     frames,
		Delay:     make([]int, len(frames)),
		Disposal:  make([]byte, len(frames)),
		LoopCount: -1, // play once and hold the final frame; no jump backwards
		Config:    image.Config{ColorModel: t.palette, Width: t.w, Height: t.h},
	}
	if loop {
		g.LoopCount = 0
	}
	for i := range frames {
		g.Delay[i] = 100
		g.Disposal[i] = gif.DisposalNone
	}
	var buf bytes.Buffer
	buf.Grow(32 << 10)
	err := gif.EncodeAll(&buf, g)
	return buf.Bytes(), err
}

// --- drawing helpers -----------------------------------------------------------

func fx(v float64) fixed.Int26_6 { return fixed.Int26_6(math.Round(v * 64)) }

func drawText(dst draw.Image, f font.Face, s string, x, y, tracking float64, c color.RGBA) {
	dr := font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: f, Dot: fixed.Point26_6{X: fx(x), Y: fx(y)}}
	if tracking == 0 {
		dr.DrawString(s)
		return
	}
	for _, r := range s {
		dr.DrawString(string(r))
		dr.Dot.X += fx(tracking)
	}
}

func roundRect(dst *image.RGBA, x, y, w, h, r float64, c color.RGBA) {
	r = math.Min(r, math.Min(w, h)/2)
	const k = 0.5522847498 // cubic Bézier circle constant
	z := vector.NewRasterizer(dst.Bounds().Dx(), dst.Bounds().Dy())
	z.DrawOp = draw.Over
	f := func(v float64) float32 { return float32(v) }
	z.MoveTo(f(x+r), f(y))
	z.LineTo(f(x+w-r), f(y))
	z.CubeTo(f(x+w-r+r*k), f(y), f(x+w), f(y+r-r*k), f(x+w), f(y+r))
	z.LineTo(f(x+w), f(y+h-r))
	z.CubeTo(f(x+w), f(y+h-r+r*k), f(x+w-r+r*k), f(y+h), f(x+w-r), f(y+h))
	z.LineTo(f(x+r), f(y+h))
	z.CubeTo(f(x+r-r*k), f(y+h), f(x), f(y+h-r+r*k), f(x), f(y+h-r))
	z.LineTo(f(x), f(y+r))
	z.CubeTo(f(x), f(y+r-r*k), f(x+r-r*k), f(y), f(x+r), f(y))
	z.ClosePath()
	z.Draw(dst, dst.Bounds(), image.NewUniform(c), image.Point{})
}

func lerp(a, b color.RGBA, t float64) color.RGBA {
	m := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 255}
}

func buildPalette(first color.RGBA, ramps [][2]color.RGBA) color.Palette {
	const steps = 24
	seen := map[color.RGBA]bool{}
	pal := color.Palette{}
	add := func(c color.RGBA) {
		if !seen[c] && len(pal) < 256 {
			seen[c] = true
			pal = append(pal, c)
		}
	}
	add(first)
	for _, r := range ramps {
		for i := 0; i <= steps; i++ {
			add(lerp(r[0], r[1], float64(i)/steps))
		}
	}
	return pal
}

func quantize(src *image.RGBA, pal color.Palette) *image.Paletted {
	out := image.NewPaletted(src.Rect, pal)
	memo := map[uint32]uint8{}
	for i, j := 0, 0; i < len(src.Pix); i, j = i+4, j+1 {
		k := uint32(src.Pix[i])<<16 | uint32(src.Pix[i+1])<<8 | uint32(src.Pix[i+2])
		idx, ok := memo[k]
		if !ok {
			idx = uint8(pal.Index(color.RGBA{src.Pix[i], src.Pix[i+1], src.Pix[i+2], 255}))
			memo[k] = idx
		}
		out.Pix[j] = idx
	}
	return out
}

func inkBounds(m *image.Alpha) image.Rectangle {
	b := image.Rectangle{}
	for y := 0; y < m.Rect.Dy(); y++ {
		for x := 0; x < m.Rect.Dx(); x++ {
			if m.Pix[y*m.Stride+x] > 0 {
				b = b.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return b
}
