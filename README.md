# Tick — email countdown timer engine (prototype)

A single Go binary that serves live countdown GIFs for email, plus a builder UI for creating them.

```
go mod tidy
go run .                       # builder at http://localhost:8080 (dev mode, unsigned URLs)
TICK_SECRET=change-me TICK_BASE_URL=https://t.yourdomain.com go run .   # production mode
```

Render a single GIF from the command line:

```
go run . render "t=volt&end=2026-11-28T05:00:00Z" out.gif
```

## How it works

Email clients don't run JavaScript, so the timer is an `<img>` whose URL hits `/t.gif` on every open. The server works out how many seconds are left and returns an animated GIF covering the next 60 seconds.

- **Template per design.** The background, boxes, labels and separators are rendered once and quantized to a palette built from the design's own color ramps. Each digit 0–9 is pre-rasterized as an alpha mask. Templates don't depend on timing, so every campaign with the same look shares one.
- **Per request, only digits are drawn.** Each frame copies the masks into their cells through a coverage→palette lookup table. There's no font rasterization or color quantization on the hot path.
- **Delta frames.** Frame 1 is a complete image, which matters because Outlook for Windows only shows the first frame. Every later frame is a sub-rectangle cropped to the ink bounds of the digits that changed.
- **Burst cache with request collapsing.** Opens that arrive in the same second for the same design trigger one render; concurrent requests wait on it and share the result.
- **No looping by default.** The animation plays once and holds its last frame, so the timer never jumps back a minute. Add `loop=1` to change that.

Measured on 2 vCPU: a 960×264 60-frame GIF renders in about 5.6 ms and comes out at 45–55 KB.

## URL parameters

| Key | Meaning | Default |
|---|---|---|
| `end` | Deadline (RFC3339 or unix seconds) | — |
| `dur` + `from` | Evergreen: window length (`48h`, `90m`) from a per-subscriber timestamp | `from` missing or invalid → starts at open |
| `t` | Preset: `midnight` `paper` `volt` `minimal` `terminal` | `midnight` |
| `bg` `box` `fg` `lc` `sc` | Background, box, digit, label and colon colors (hex) | preset |
| `font` | `inter` `poppins` `serif` `mono` | preset |
| `u` | Units to show, any subset of `dhms`; the largest unit absorbs the rest | `dhms` |
| `l` | Labels for d,h,m,s, comma separated | English |
| `boxes` `sep` `uc` `loop` | 0/1 toggles | preset |
| `r` | Corner radius as a fraction of box height (0–0.5) | preset |
| `w` | Display width in CSS px (200–800); rendered at `x`× density | 480 |
| `x` | Pixel density | 2 |
| `f` | Frames (seconds) per GIF | 60 |
| `exp` | Text shown after the deadline | zeros |
| `sig` | HMAC signature (required when `TICK_SECRET` is set) | — |

**Signing.** `GET /api/embed?<params>&link=...` returns a signed URL and ready-to-paste HTML. The signature covers every parameter except `from`, so an ESP merge tag can be substituted after signing. Repeated keys are rejected.

## Known platform limits (true for every vendor)

- **Apple Mail Privacy Protection** may prefetch the image at delivery, so some Apple Mail opens will show a stale time.
- **Outlook for Windows** shows only the first frame. The first frame is always correct, so the timer stays accurate there but doesn't animate.

## Production roadmap

1. Put a CDN in front with a 1 s edge TTL keyed on the full URL. Replace the template map with a real LRU, and add metrics.
2. Accounts and multi-tenancy: a timers table so URLs become `/t/{id}.gif`. That gives short URLs and lets you edit a design after it's sent.
3. Custom font upload per client, background images and gradients (needs adaptive palettes), and a brand kit.
4. AI brand match: paste a site URL and get colors, typeface and copy.
5. Analytics: opens per timer, client and device mix, and the open curve relative to the deadline.
