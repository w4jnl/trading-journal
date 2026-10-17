# trading-journal brand assets

The repository's face on GitHub and in the README. The mark is the one the web app and the
phone app already carry (`web/static/img/brand/`): four rounded squares, one large and three
small, the way fills gather into a trade. The banners follow flok's and vink's layouts, so the
three repositories read as one family: dark ground, a 6 px bar in the tool's colour down the left,
a JetBrains Mono wordmark.

## Files

| file | use |
| --- | --- |
| `trading-journal-mark.svg` | the mark in teal, transparent background (copy of the app's `logo-mark.svg`) |
| `trading-journal-mark-white.svg` | the mark in white, for teal or dark surfaces |
| `trading-journal-lockup.svg` | mark + `trading-journal` wordmark, horizontal |
| `trading-journal-lockup-tagline.svg` | the lockup with "self-hosted journal for futures traders" |
| `trading-journal-hero.png` | README banner, 1280×320 |
| `trading-journal-social.png` | GitHub social preview, 1280×640 (repository Settings › Social preview) |
| `trading-journal-avatar.png` | 460×460 teal tile with the white mark, for a profile or organisation avatar |
| `hero.html`, `social.html`, `avatar.html` | the pages the PNGs are rendered from |
| `render.sh` | renders the three PNGs with headless Chrome |

The lockup SVGs keep their text as text and name the font, so they need JetBrains Mono where they
are shown. The PNGs are what GitHub and the README use.

## Palette

| token | value | role |
| --- | --- | --- |
| ground | `#14181A` | the banners' background (the family's) |
| teal | `#12999D` | the mark, the bar, links in the light theme |
| teal, dark theme | `#2EBABE` | the accent in the app's dark theme, "plan" in the legend |
| ink | `#F2F5F5` | wordmark |
| muted | `#8A999C` | tagline, the repo line |
| win | `#22c55e` | a positive P&L, in the app's dark theme |
| loss | `#ef4444` | a negative P&L |
| open position | `#60a5fa` | an open position, the info colour |
| nudge | `#f59e0b` | a guardrail nudge, the warn colour |

The app's full token set, light and dark, is in `web/static/css/app.css`; the phone app's in
`app/ios/Shared/Brand.swift`.

## Type

JetBrains Mono 700 for the wordmark, 400 for the tagline and the repo line; Inter 600 for the
social card's two-line tagline, as in flok. Both faces are the repo's own variable woff2 files in
`web/static/fonts/`, so rendering needs no network.

## Re-rendering

```sh
assets/brand/render.sh        # trading-journal-hero.png, -social.png, -avatar.png
```

Needs Google Chrome (set `CHROME` to another Chromium). The pages load the fonts over `file://`,
which is why the script passes `--allow-file-access-from-files`. After a change to the tagline,
edit `hero.html` and `trading-journal-lockup-tagline.svg` together.

## Where the files go on GitHub

- Social preview: repository Settings › General › Social preview › Upload an image:
  `trading-journal-social.png`.
- The avatar is for an organisation or profile picture, should one be wanted; the repository
  itself has no image of its own.
- The README shows `trading-journal-hero.png` at width 800.
