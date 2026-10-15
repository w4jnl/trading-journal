# Trading Journal for iPhone and iPad

The journal's companion: today's P&L and open positions live, the trade log, the journal, the
calendar, the guardrail's nudges, equity and drawdown. SwiftUI, iOS 18 and later, iPhone and
iPad. It talks to the journal's HTTP API only ([API.md](../../API.md)), with a **device key**
made by pairing.

## Build

```sh
brew install xcodegen
make ios-project          # app/ios/TradingJournal.xcodeproj from project.yml (not committed)
make ios-build            # a Debug build for the iPhone 17 simulator, no signing
make ios-test             # the unit tests on that simulator
open app/ios/TradingJournal.xcodeproj
```

The simulator runs the app against a real journal. Pair it like a phone (the QR cannot be
scanned there; type the server and the code), or start it configured:

```sh
xcrun simctl launch booted nl.w4j.tradingjournal -serverURL http://127.0.0.1:18777 -apiKey tj_… [-open trade/5]
```

`-open` takes a route: `today`, `day/2026-10-14`, `trade/<id>`, `position/<account>/<symbol>`,
`journal/<id>`, `nudges`, `plan`, `accounts`, `settings`. The same routes open from
`tj://open/<route>` (`xcrun simctl openurl booted tj://open/accounts`).

`SERVER=http://… KEY=tj_… SHOTS=/dir make ios-screens` walks the tabs and writes a PNG per
screen (the `TJScreens` scheme, so `make ios-test` never runs it). `SERVER=… KEY=… make
ios-fixtures` recaptures `TJTests/Fixtures/*.json` from a journal: the wire tests decode them.

## Pairing

In the web app: Settings → Phone app → *Pair a phone* shows a code and a QR of
`tj://pair?server=…&code=…` for five minutes. The app scans it (or takes the code typed),
exchanges it on `POST /api/v1/pair` for a key of kind `device`, and keeps the key in the
keychain group it shares with its extensions. *Unpair* on the web revokes the key; *Sign out*
in the app forgets it on the phone.

## On a phone

1. Copy `Local.xcconfig.example` to `Local.xcconfig` and put your Apple team id in it. A free
   Apple ID installs on your own phone from Xcode (set `TJ_ENTITLEMENTS =` empty: its personal
   team cannot sign push); the Developer Program adds push, widgets and TestFlight.
2. `make ios-device` with the phone unlocked on a cable.
3. `make ios-testflight` archives and uploads (the app record `nl.w4j.tradingjournal` must exist
   in App Store Connect). The marketing version follows the server's release.

## Layout

- `Shared/`: compiled into the app and, in later phases, every extension. `Wire.swift` (the
  API's JSON as Swift types, one decoder), `API.swift` (`TJAPI`: one bearer key, JSON in and
  out), `Keychain.swift`, `Brand.swift` (the web app's tokens, Inter and JetBrains Mono from
  `Fonts/`), `Formatting.swift`, `Markdown.swift`, `Route.swift`.
- `TradingJournal/Model/`: `Client` (Today over the SSE stream, reads, actions), `Settings`.
- `TradingJournal/Views/`: one file per screen; `Components.swift` holds the cards, pills, P&L
  chips, stat strips, the heat cell and the authenticated image.
- `TJTests/`: Swift Testing over the captured fixtures and the pure helpers. `TJScreens/`: the
  screenshot walker.

## What is in this build

Phases 1 and 2 of the plan: pairing, the read-only screens (Today, positions, trades,
journal, calendar, accounts and equity, nudges, the day plan, settings), and the writes from
the phone: journal edit, delete, append and the first note on a trade or a day, photos as
attachments, the day plan, the plan on an open position with the risk mirrored live, the
trader's switches, balance anchors. A save that meets an AI review in progress offers to
append instead; one that meets a changed entry offers to reload or overwrite.

`SERVER=http://… KEY=tj_… make ios-test` also runs the live write tests against that
journal (they put back what they change). Push notifications, the Session Live Activity and
the widgets follow in the next phases; the server already serves all of it.
