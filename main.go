package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

type server struct {
	secret  []byte // empty = dev mode (unsigned URLs accepted)
	baseURL string
	cache   *burstCache
}

func main() {
	if err := loadFonts(); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "render" { // tick render "<query>" out.gif
		cliRender(os.Args[2], os.Args[3])
		return
	}
	s := &server{
		secret:  []byte(os.Getenv("TICK_SECRET")),
		baseURL: envOr("TICK_BASE_URL", "http://localhost:8080"),
		cache:   newBurstCache(3 * time.Second),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t.gif", s.timer)
	mux.HandleFunc("GET /api/embed", s.embed)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	addr := ":" + envOr("PORT", "8080")
	mode := "signed URLs required"
	if len(s.secret) == 0 {
		mode = "DEV MODE: unsigned URLs accepted (set TICK_SECRET in production)"
	}
	log.Printf("tick listening on %s — %s", addr, mode)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// GET /t.gif — the hot path. Every email open lands here.
func (s *server) timer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, vs := range q {
		if len(vs) > 1 { // never let a repeated key smuggle a second value past the signature
			http.Error(w, "duplicate parameter", http.StatusBadRequest)
			return
		}
	}
	if len(s.secret) > 0 && !verify(s.secret, q) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}
	d, err := parseDesign(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	start := time.Now()
	remaining := int64(time.Until(d.Deadline(start)) / time.Second)
	if remaining < 0 {
		remaining = 0
	}

	// Opens arrive in bursts right after a send. Identical (design, second)
	// pairs collapse into one render; concurrent callers wait on it.
	key := canonical(q) + "|" + q.Get("from") + "|" + strconv.FormatInt(remaining, 10)
	body, hit, err := s.cache.get(key, func() ([]byte, error) { return Render(d, remaining) })
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "image/gif")
	// Image proxies (Gmail, Yahoo) and browsers must re-fetch on every open.
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0, private")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	h.Set("X-Render-Ms", fmt.Sprintf("%.2f", float64(time.Since(start).Microseconds())/1000))
	h.Set("X-Cache", map[bool]string{true: "HIT", false: "MISS"}[hit])
	w.Write(body)
}

// GET /api/embed — returns a signed URL plus copy-paste HTML. In production
// this sits behind client auth; the builder UI calls it for live previews.
func (s *server) embed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	link := q.Get("link")
	alt := q.Get("alt")
	q.Del("link")
	q.Del("alt")
	q.Del("sig")
	d, err := parseDesign(q)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(s.secret) > 0 {
		q.Set("sig", sign(s.secret, q))
	}
	// Merge tags like {{ person.created }} must survive un-escaped so the ESP
	// can substitute them, so `from` is appended raw and is never signed.
	from := q.Get("from")
	q.Del("from")
	src := s.baseURL + "/t.gif?" + q.Encode()
	if from != "" {
		src += "&from=" + from
	}

	digits := make([]int, len(d.Units))
	for i := range digits {
		digits[i] = 2
	}
	g := measure(d, digits)
	dispW, dispH := d.Width, int(float64(g.H)/float64(d.Scale)+0.5)
	if alt == "" {
		alt = "Countdown timer"
	}
	img := fmt.Sprintf(`<img src="%s" width="%d" height="%d" alt="%s" style="display:block;border:0;outline:none;width:100%%;max-width:%dpx;height:auto;margin:0 auto;">`,
		html.EscapeString(src), dispW, dispH, html.EscapeString(alt), dispW)
	snippet := img
	if link != "" {
		snippet = fmt.Sprintf(`<a href="%s" target="_blank" style="text-decoration:none;">%s</a>`, html.EscapeString(link), img)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"url": src, "html": snippet, "width": dispW, "height": dispH, "signed": len(s.secret) > 0,
	})
}

// --- burst cache with request collapsing -------------------------------------

type entry struct {
	done    chan struct{}
	body    []byte
	err     error
	expires time.Time
}

type burstCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*entry
}

func newBurstCache(ttl time.Duration) *burstCache {
	c := &burstCache{ttl: ttl, m: map[string]*entry{}}
	go func() {
		for range time.Tick(ttl) {
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.m {
				if !e.expires.IsZero() && now.After(e.expires) {
					delete(c.m, k)
				}
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *burstCache) get(key string, fill func() ([]byte, error)) ([]byte, bool, error) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok {
		c.mu.Unlock()
		<-e.done
		return e.body, true, e.err
	}
	e := &entry{done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	e.body, e.err = fill()
	c.mu.Lock()
	e.expires = time.Now().Add(c.ttl)
	if e.err != nil {
		delete(c.m, key)
	}
	c.mu.Unlock()
	close(e.done)
	return e.body, false, e.err
}

// --- misc ---------------------------------------------------------------------

func cliRender(query, out string) {
	q, err := url.ParseQuery(query)
	if err != nil {
		log.Fatal(err)
	}
	d, err := parseDesign(q)
	if err != nil {
		log.Fatal(err)
	}
	rem := int64(time.Until(d.Deadline(time.Now())) / time.Second)
	b, err := Render(d, rem)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s  %d bytes\n", out, len(b))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
