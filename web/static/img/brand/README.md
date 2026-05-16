# Brand chrome assets

Sourced from `exportData/design_handoff_w4j_trading_journal/assets/`.
Files in this directory are embedded into the binary via
`web/web.go` (`StaticFS`) and served from `/static/img/brand/`.

| File                          | Purpose                                                          |
|-------------------------------|------------------------------------------------------------------|
| `logo-mark.svg`               | Full 4-square teal mark on transparent bg (kept as a source).    |
| `logo-mark-white.svg`         | White variant for teal surfaces.                                 |
| `favicon.svg`                 | Simplified 3-square mark, browser tab favicon (`type="image/svg+xml"`). |
| `apple-touch-icon.svg`        | 180x180 teal squircle + white mark for iOS home-screen.          |
| `lockup-trading-journal.svg`  | Horizontal mark + wordmark on paper bg.                          |
| `og-image.svg`                | 1200x630 Open Graph share card (light variant).                  |

## PNG fallback (optional)

Modern browsers and most social scrapers handle SVG, but iOS
Springboard and some older clients do not. To generate raster
fallbacks once and commit them alongside the SVGs:

```bash
cd web/static/img/brand

# Favicon raster pair + .ico bundle
rsvg-convert -w 16  favicon.svg          > favicon-16.png
rsvg-convert -w 32  favicon.svg          > favicon-32.png
convert favicon-16.png favicon-32.png    favicon.ico

# iOS home-screen icon
rsvg-convert -w 180 apple-touch-icon.svg > apple-touch-icon.png

# Open Graph share card (Twitter rejects SVG)
rsvg-convert -w 1200 og-image.svg        > og-image.png
```

Requires `librsvg` (`brew install librsvg`) and `imagemagick`
(`brew install imagemagick`). Outfit + JetBrains Mono should be
installed system-wide for the OG render to match the spec; otherwise
the SVG falls back to `system-ui`.

Once the PNGs are committed, add these tags **above** the SVG link
tags in `web/templates/base.html` so older clients pick the raster
without disturbing modern browsers:

```html
<link rel="icon" sizes="any" href="/static/img/brand/favicon.ico">
<link rel="apple-touch-icon" href="/static/img/brand/apple-touch-icon.png">
```

And swap `og:image` to point at `og-image.png`.
