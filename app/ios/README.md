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
ios-fixtures` recaptures `TJTests/Fixtures/*.json` from a journal and scrubs them (`scripts/scrub-fixtures.py`: example account ids, scaled capital, placeholder text): the wire tests decode them.

## Pairing

In the web app: Settings → Phone app → *Pair a phone* shows a code and a QR of
`tj://pair?server=…&code=…` for five minutes. The app scans it (or takes the code typed),
exchanges it on `POST /api/v1/pair` for a key of kind `device`, and keeps the key in the
keychain group it shares with its extensions. *Unpair* on the web revokes the key; *Sign out*
in the app forgets it on the phone.

## On a phone

1. Copy `Local.xcconfig.example` to `Local.xcconfig` and put your Apple team id in it, plus an
   App Store Connect API key (role Admin) so the command line signs in without Xcode's account.
   A free Apple ID installs on your own phone from Xcode (set `TJ_ENTITLEMENTS =` empty: its
   personal team cannot sign push); the Developer Program adds push, widgets and TestFlight.
2. The first build registers the App ID `nl.w4j.tradingjournal` with Push Notifications and App
   Groups by itself. Two things the API cannot register, done once in the developer portal
   (Certificates, Identifiers & Profiles): under **Identifiers → App Groups** register
   `group.nl.w4j.tradingjournal`; under **Identifiers → App IDs → nl.w4j.tradingjournal** tick
   **Time Sensitive Notifications** and, next to App Groups, *Configure* and tick the group.
   Until then sign with `TradingJournal.basic.entitlements` (see `Local.xcconfig.example`):
   push and the keychain group, no widgets. The widgets' own App ID,
   `nl.w4j.tradingjournal.Widgets`, needs the same group assigned (the first signed build
   registers the App ID; then App IDs → it → App Groups → Configure); until then leave
   `TJ_WIDGET_ENTITLEMENTS` empty and the widgets show their placeholder.
3. `make ios-device` with the phone unlocked on a cable.
4. `make ios-testflight` archives and uploads (the app record `nl.w4j.tradingjournal` must exist
   in App Store Connect). The marketing version follows the server's release.

## Notifications

Settings → Notifications asks once, hands the APNs token to the journal (`POST /api/v1/devices`,
once per server and token) and decides what this phone gets: each kind on or off, its sound
(the journal's two, bundled from `TradingJournal/Sounds`, the iOS default, or silent), whether it
breaks through Focus, closed trades only in regular hours, and the Live Activity choices. The
server keeps a copy per phone and filters before every push. Two extensions dress the push:
`TJNotificationService` puts the trade's chart on a closed-trade push, `TJNotificationContent`
is the expanded card (`Shared/NotificationCard.swift`) that fetches the live numbers and marks a
nudge seen in place. Both read the server's address and the key from the keychain group the app
shares, so they work once the app ran after an update. On the Lock Screen a nudge offers Seen
and Open, a closed trade Note… (dictation included) and Open; a swipe away marks a nudge seen;
the server's silent sync takes seen nudges off the Lock Screen and keeps the badge right.

In the simulator a payload file stands in for APNs, extensions included:

```sh
xcrun simctl push booted nl.w4j.tradingjournal app/ios/TJTests/Payloads/nudge.apns
```

`TJTests/Payloads/` has one per kind: `nudge`, `day-nudge`, `trade-closed`, `plan-missing`, `sync`.
`simctl` refuses the silent `sync` (no visible content): that path, and the real APNs round trip,
are checked on a phone, where `make ios-device` installs a build that registers a sandbox token.
`SHOTS=/dir PUSH=1 make ios-screens` with `SHOTS=/dir app/ios/TJScreens/push-driver.sh` beside it
walks the notification flow in the simulator and photographs the banner and the expanded card.

## Live Activity and widgets

`TJWidgets` is the Session Live Activity (Dynamic Island, Lock Screen) and the Today widgets.
`Shared/SessionActivity.swift` holds the attributes and the content state the server pushes
(API.md, "The Session activity"), `Shared/LiveViews.swift` the faces. `LiveActivities.swift`
hands the server the push-to-start token and each activity's update token, starts the Session
itself when the app is open and the day has begun, refreshes it from Today while the app is in
front, and ends it when the user switches it off. The server starts it on the first fill of the
day, updates it on every change and ends it when the day is flat and quiet. While it shows, a
closed trade lights it up with its sound instead of a separate notification (Settings →
Notifications → "Closed trades as notifications too" turns that off). The Today widgets
(`accessoryCircular`, `accessoryRectangular`, `accessoryInline`, `systemSmall`, `systemMedium`)
read `Shared/TodayCache.swift`, which the app writes to the App Group after every fetch, and
reload then and on the silent sync. An unsigned simulator build has no APS environment: the
activity still starts locally and follows the app, but no push reaches it.

## Privacy

Settings → Privacy. **Require Face ID** (the label follows the enrolled biometry) is off by
default; on, the app asks when it comes back after **Lock after** (Immediately, 30 seconds, 1,
5, 15 minutes, 1 hour; 1 minute by default) and at every cold start unless **Lock on launch** is
off. The policy is `deviceOwnerAuthentication`, so the passcode is the fallback and the lock
cannot shut the owner out; a phone without a passcode cannot turn it on. Turning it on or off
takes one authentication. While the app is merely inactive (the switcher, a call) the content
is covered without a prompt. The lock is a UI gate (`LockGate`, a window above the alert
level; the pure decision is `LockPolicy.shouldLock`, measured on `ContinuousClock` so a phone
asleep in a pocket still counts): the device key stays in the shared keychain as before, the
client keeps streaming behind the cover, and the server-side control remains revoking the
device from the web app. **Hide amounts until unlocked** puts `.privacySensitive` on the money
in the widgets, the Live Activity and the notification cards; the phone's own unlock reveals
them, and the activity follows at its next update.

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

All five phases of the plan: pairing, the read-only screens (Today, positions, trades, journal,
calendar, accounts and equity, nudges, the day plan, the weekly digest with the lessons,
settings), the writes from the phone (journal edit, delete, append and the first note on a
trade or a day, photos as attachments, the AI review's suggestions accepted or dismissed, the
day plan, the plan on an open position with the risk mirrored live, the trader's switches,
balance anchors), push (registration, the three categories with their Lock Screen actions, the
service and content extensions, the settings mirrored to the server, the badge and the silent
sync), the Session Live Activity with the Today widgets and the guardrail switch in Control
Center, and the iPad: a sidebar, three columns for the trades and the journal, ⌘R refresh, ⌘N a
day note, ⌘F the symbol filter. For a user with an AI provider in the web app's Settings
(`GET /me` says `ai.ready`), the AI from the phone: Analyze on a note, on a trade without one
and in the first-note sheet, the day run on a day's page, a stop and target for an open position, a draft of today's
plan; every call runs on the server and the result arrives over the event stream.

`SERVER=http://… KEY=tj_… make ios-test` also runs the live write tests against that
journal (they put back what they change). `IOS_SCREENS_SIM='platform=iOS Simulator,name=iPad
Pro 11-inch (M5)' make ios-screens …` walks the iPad. `.github/workflows/ios.yml` runs the unit
tests on a macOS runner when the app changes.
