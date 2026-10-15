# The one front door: the Go server and the iOS app, the same commands on a laptop and in CI.
BIN := trading-journal
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet smoke ios-project ios-build ios-test ios-screens ios-device ios-testflight ios-fixtures

build:
	go build -o $(BIN) ./cmd/server

test:
	go test -race ./...

vet:
	go vet ./...

# The route smoke driver (see .claude/skills/run-trading-journal); never on 8080.
smoke:
	PORT=18098 .claude/skills/run-trading-journal/driver.sh

# --- iOS (app/ios) -----------------------------------------------------------------------
# brew install xcodegen. The project is generated, never committed; Local.xcconfig holds your team.
IOS_SIM ?= platform=iOS Simulator,name=iPhone 17
IOS_SCREENS_SIM ?= platform=iOS Simulator,name=iPhone 17
ios_conf = $(shell sed -n 's/^$(1)[[:space:]]*=[[:space:]]*//p' app/ios/Local.xcconfig 2>/dev/null)
ASC_KEY_ID ?= $(call ios_conf,TJ_ASC_KEY_ID)
ASC_ISSUER_ID ?= $(call ios_conf,TJ_ASC_ISSUER_ID)
ASC_KEY_PATH ?= $(HOME)/.appstoreconnect/private_keys/AuthKey_$(ASC_KEY_ID).p8
ASC_AUTH = $(if $(ASC_KEY_ID),-authenticationKeyPath '$(ASC_KEY_PATH)' -authenticationKeyID $(ASC_KEY_ID) -authenticationKeyIssuerID $(ASC_ISSUER_ID))
XCB = xcodebuild -project TradingJournal.xcodeproj -derivedDataPath build/DerivedData

ios-project:
	@[ -f app/ios/Local.xcconfig ] || printf 'TJ_TEAM_ID =\nTJ_ENTITLEMENTS = TradingJournal/TradingJournal.entitlements\n' > app/ios/Local.xcconfig
	cd app/ios && xcodegen generate
ios-build: ios-project
	cd app/ios && $(XCB) -scheme TradingJournal -configuration Debug -destination '$(IOS_SIM)' build CODE_SIGNING_ALLOWED=NO -quiet
ios-test: ios-project
	cd app/ios && $(XCB) -scheme TradingJournal -configuration Debug -destination '$(IOS_SIM)' test CODE_SIGNING_ALLOWED=NO -quiet
# Screenshots of every tab against a running journal: SERVER=http://… KEY=tj_… SHOTS=/dir make ios-screens
ios-screens: ios-project
	cd app/ios && TEST_RUNNER_TJ_SERVER='$(SERVER)' TEST_RUNNER_TJ_KEY='$(KEY)' TEST_RUNNER_TJ_SHOTS='$(SHOTS)' \
	$(XCB) -scheme TJScreens -configuration Debug -destination '$(IOS_SCREENS_SIM)' test CODE_SIGNING_ALLOWED=NO -quiet
# Build, install and launch on the iPhone (unlocked; cable, or Wi-Fi once paired); signs with Local.xcconfig's team.
ios-device: ios-project
	@udid=$$(xcrun devicectl list devices 2>/dev/null | grep -v simulated | grep -E ' connected | available \(paired\) ' | grep -oE '[0-9A-F]{8}-[0-9A-F]{16}' | head -1); \
	[ -n "$$udid" ] || { echo "ios-device: no iPhone reachable: unlock it, on a cable or on the same Wi-Fi"; exit 1; }; \
	cd app/ios && $(XCB) -scheme TradingJournal -configuration Debug -destination "id=$$udid" -allowProvisioningUpdates $(ASC_AUTH) build -quiet && \
	xcrun devicectl device install app --device "$$udid" build/DerivedData/Build/Products/Debug-iphoneos/TradingJournal.app >/dev/null && \
	xcrun devicectl device process launch --terminate-existing --device "$$udid" nl.w4j.tradingjournal >/dev/null && echo "ios-device: installed and launched on $$udid"
# TestFlight: archive a Release build and upload it (the app record for the bundle id must exist in
# App Store Connect; ExportOptions.plist lets Xcode raise the build number).
ios-testflight: ios-project
	cd app/ios && rm -rf build/TradingJournal.xcarchive build/export && \
	$(XCB) -scheme TradingJournal -configuration Release -destination 'generic/platform=iOS' -archivePath build/TradingJournal.xcarchive -allowProvisioningUpdates $(ASC_AUTH) archive -quiet && \
	xcodebuild -exportArchive -archivePath build/TradingJournal.xcarchive -exportOptionsPlist ExportOptions.plist -exportPath build/export -allowProvisioningUpdates $(ASC_AUTH)
# Capture the test fixtures from a running journal: SERVER=http://… KEY=tj_… make ios-fixtures
ios-fixtures:
	@[ -n "$(SERVER)" ] && [ -n "$(KEY)" ] || { echo "ios-fixtures: SERVER=http://host:port KEY=tj_… make ios-fixtures"; exit 1; }
	@F=app/ios/TJTests/Fixtures; H="Authorization: Bearer $(KEY)"; B="$(SERVER)/api/v1"; \
	get() { curl -sf -H "$$H" -o "$$F/$$2" -w "$$2 %{size_download}B\n" "$$B$$1"; }; \
	get /me me.json && get /today today.json && get '/trades?page=1' trades.json && get /positions positions.json && \
	get '/journal?limit=6' journal.json && get '/stats/daily?from=2026-09-01&to=today' stats_daily.json && get /nudges nudges.json && \
	get /accounts accounts.json && get /equity equity.json && get /journal/labels journal_labels.json && get /strategies strategies.json && get /version version.json && \
	TID=$$(python3 -c "import json;print(json.load(open('$$F/trades.json'))['items'][0]['id'])") && get /trades/$$TID trade_detail.json && \
	JID=$$(python3 -c "import json;d=json.load(open('$$F/journal.json'))['items'];print(d[0]['id'] if d else '')") && [ -n "$$JID" ] && get /journal/$$JID journal_detail.json; \
	python3 -c "import json;[json.dump({**json.load(open('$$F/'+f+'.json')),'chart':{'status':None,'panels':[]}},open('$$F/'+f+'.json','w'),indent=1,ensure_ascii=False) for f in ('trade_detail','journal_detail')]"
