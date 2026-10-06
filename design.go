package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/color"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Design is a fully resolved, validated timer specification. Everything a
// render needs comes from here, so two requests with equal Designs (and equal
// seconds-remaining) produce byte-identical GIFs — which is what makes burst
// caching safe.
type Design struct {
	// Timing: either a fixed deadline, or an evergreen window that starts at
	// a per-subscriber timestamp (From) supplied via an ESP merge tag.
	End  time.Time
	From time.Time
	Dur  time.Duration

	// Look
	Font     string // inter | poppins | serif | mono
	Units    string // ordered subset of "dhms"
	Labels   []string
	Upper    bool
	Boxes    bool
	Radius   float64 // box corner radius as a fraction of box height (0..0.5)
	Sep      bool    // draw ":" separators between units
	BG       color.RGBA
	Box      color.RGBA
	FG       color.RGBA
	Label    color.RGBA
	SepColor color.RGBA

	// Output
	Width  int  // display width in CSS px (the <img width>)
	Scale  int  // pixel density; 2 = retina
	Frames int  // seconds of animation per request
	Loop   bool // false = animation stops on its last frame

	// Expired state
	Expired string
}

// Preset themes. Any field can be overridden by query params.
var presets = map[string]url.Values{
	"midnight": {"bg": {"0b0d12"}, "box": {"1a1f2b"}, "fg": {"ffffff"}, "lc": {"8a93a6"}, "sc": {"3b4252"}, "font": {"inter"}, "boxes": {"1"}, "r": {"0.18"}},
	"paper":    {"bg": {"faf7f2"}, "box": {"ffffff"}, "fg": {"1c1a17"}, "lc": {"8c857a"}, "sc": {"d8d2c7"}, "font": {"serif"}, "boxes": {"1"}, "r": {"0.08"}},
	"volt":     {"bg": {"0a0a0a"}, "box": {"d7ff3a"}, "fg": {"0a0a0a"}, "lc": {"d7ff3a"}, "sc": {"d7ff3a"}, "font": {"poppins"}, "boxes": {"1"}, "r": {"0.5"}},
	"minimal":  {"bg": {"ffffff"}, "box": {"ffffff"}, "fg": {"111111"}, "lc": {"6b6b6b"}, "sc": {"c4c4c4"}, "font": {"inter"}, "boxes": {"0"}, "sep": {"1"}},
	"terminal": {"bg": {"050806"}, "box": {"0c1a10"}, "fg": {"39ff88"}, "lc": {"2a8f55"}, "sc": {"1d5e38"}, "font": {"mono"}, "boxes": {"1"}, "r": {"0.06"}},
}

// Keys that are allowed to vary per subscriber and are therefore excluded
// from the signature (an ESP substitutes them after we sign the URL).
var unsignedKeys = map[string]bool{"sig": true, "from": true}

func parseDesign(q url.Values) (*Design, error) {
	// Layer: preset defaults, then explicit params.
	v := url.Values{}
	base := presets["midnight"]
	if p, ok := presets[q.Get("t")]; ok {
		base = p
	}
	for k, vs := range base {
		v[k] = vs
	}
	for k, vs := range q {
		v[k] = vs
	}

	d := &Design{
		Font:    pick(v.Get("font"), "inter", "inter", "poppins", "serif", "mono"),
		Upper:   v.Get("uc") != "0",
		Boxes:   v.Get("boxes") != "0",
		Sep:     v.Get("sep") == "1",
		Loop:    v.Get("loop") == "1",
		Expired: limit(v.Get("exp"), 60),
	}
	var err error
	if d.BG, err = hexColor(v.Get("bg")); err != nil {
		return nil, err
	}
	if d.Box, err = hexColor(v.Get("box")); err != nil {
		return nil, err
	}
	if d.FG, err = hexColor(v.Get("fg")); err != nil {
		return nil, err
	}
	if d.Label, err = hexColor(v.Get("lc")); err != nil {
		return nil, err
	}
	if d.SepColor, err = hexColor(v.Get("sc")); err != nil {
		return nil, err
	}
	if !d.Boxes {
		d.Box = d.BG
	}

	d.Radius = clampF(parseF(v.Get("r"), 0.15), 0, 0.5)
	d.Width = clampI(parseI(v.Get("w"), 480), 200, 800)
	d.Scale = clampI(parseI(v.Get("x"), 2), 1, 3)
	d.Frames = clampI(parseI(v.Get("f"), 60), 1, 90)

	d.Units = "dhms"
	if u := v.Get("u"); u != "" {
		d.Units = ""
		for _, c := range "dhms" { // canonical order regardless of input order
			if strings.ContainsRune(u, c) {
				d.Units += string(c)
			}
		}
		if d.Units == "" {
			return nil, fmt.Errorf("u must contain at least one of d,h,m,s")
		}
	}
	defaults := map[byte]string{'d': "Days", 'h': "Hours", 'm': "Minutes", 's': "Seconds"}
	custom := map[byte]string{}
	if l := v.Get("l"); l != "" {
		parts := strings.Split(l, ",")
		for i, c := range []byte("dhms") {
			if i < len(parts) {
				custom[c] = limit(strings.TrimSpace(parts[i]), 16)
			}
		}
	}
	for i := 0; i < len(d.Units); i++ {
		c := d.Units[i]
		lbl := defaults[c]
		if s, ok := custom[c]; ok {
			lbl = s
		}
		if d.Upper {
			lbl = strings.ToUpper(lbl)
		}
		d.Labels = append(d.Labels, lbl)
	}

	// Timing
	switch {
	case v.Get("end") != "":
		if d.End, err = parseTime(v.Get("end")); err != nil {
			return nil, fmt.Errorf("end: %w", err)
		}
	case v.Get("dur") != "":
		if d.Dur, err = time.ParseDuration(v.Get("dur")); err != nil || d.Dur <= 0 {
			return nil, fmt.Errorf("dur: expected a Go duration like 48h or 90m")
		}
		// An unsubstituted or garbage merge tag shouldn't break the image:
		// fall back to "starts now" so the subscriber still sees a full window.
		if t, err := parseTime(v.Get("from")); err == nil {
			d.From = t
		}
	default:
		return nil, fmt.Errorf("provide end (deadline) or dur (+ optional from) for an evergreen timer")
	}
	return d, nil
}

// Deadline resolves the moment this timer hits zero, relative to now.
func (d *Design) Deadline(now time.Time) time.Time {
	if !d.End.IsZero() {
		return d.End
	}
	start := d.From
	if start.IsZero() {
		start = now
	}
	return start.Add(d.Dur)
}

// --- signing ---------------------------------------------------------------

// canonical returns a stable representation of the signed portion of a query.
func canonical(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if !unsignedKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(q.Get(k)))
	}
	return b.String()
}

func sign(secret []byte, q url.Values) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(canonical(q)))
	return hex.EncodeToString(m.Sum(nil))[:20]
}

func verify(secret []byte, q url.Values) bool {
	got := q.Get("sig")
	return got != "" && hmac.Equal([]byte(got), []byte(sign(secret, q)))
}

// --- small parsers -----------------------------------------------------------

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 { // milliseconds
			return time.UnixMilli(n), nil
		}
		return time.Unix(n, 0), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q (use RFC3339 or unix seconds)", s)
}

func hexColor(s string) (color.RGBA, error) {
	s = strings.TrimPrefix(s, "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if len(s) != 6 || err != nil {
		return color.RGBA{}, fmt.Errorf("bad color %q", s)
	}
	return color.RGBA{uint8(n >> 16), uint8(n >> 8), uint8(n), 255}, nil
}

func pick(s, def string, allowed ...string) string {
	for _, a := range allowed {
		if s == a {
			return s
		}
	}
	return def
}

func limit(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func parseI(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func parseF(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}

func clampI(v, lo, hi int) int         { return max(lo, min(hi, v)) }
func clampF(v, lo, hi float64) float64 { return max(lo, min(hi, v)) }
