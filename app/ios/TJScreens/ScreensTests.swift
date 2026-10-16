import XCTest

/// The screenshot walker and the one smoke path, not a test of behaviour: run against a journal
/// (TJ_SERVER, TJ_KEY) it launches the app configured, opens each tab and writes a PNG per screen
/// to TJ_SHOTS. Its own scheme, so `make ios-test` never runs it.
@MainActor
final class ScreensTests: XCTestCase {
    private let env = ProcessInfo.processInfo.environment
    private lazy var shots = env["TJ_SHOTS"] ?? NSTemporaryDirectory()

    func testScreens() throws {
        let server = try XCTUnwrap(env["TJ_SERVER"], "TJ_SERVER: the journal to show")
        let key = try XCTUnwrap(env["TJ_KEY"], "TJ_KEY: a device key for it")
        let app = XCUIApplication()
        app.launchArguments = ["-serverURL", server, "-apiKey", key]
        app.launch()
        XCTAssertTrue(app.staticTexts["TODAY"].waitForExistence(timeout: 20), "Today never rendered")
        sleep(2)
        shot("today")

        for (tab, title) in [("Trades", "trades"), ("Journal", "journal"), ("Calendar", "calendar"), ("Settings", "settings")] {
            let button = app.tabBars.buttons[tab].exists ? app.tabBars.buttons[tab] : app.buttons[tab]
            if button.waitForExistence(timeout: 5) {
                button.tap()
                sleep(3)
                shot(title)
            }
        }

        // a deep link straight to a screen
        app.terminate()
        app.launchArguments = ["-serverURL", server, "-apiKey", key, "-open", "accounts"]
        app.launch()
        XCTAssertTrue(app.staticTexts["EQUITY NOW"].waitForExistence(timeout: 20), "the accounts screen never rendered")
        sleep(2)
        shot("accounts")

        // the editors: a journal note, the position plan, the day plan
        if let entry = env["TJ_ENTRY"] {
            app.terminate()
            app.launchArguments = ["-serverURL", server, "-apiKey", key, "-open", "journal/\(entry)"]
            app.launch()
            _ = app.buttons["Edit"].waitForExistence(timeout: 20)
            shot("journal-entry")
            if app.buttons["Edit"].exists {
                app.buttons["Edit"].tap()
                sleep(3)
                shot("journal-edit")
            } else {
                XCTFail("no Edit button; buttons: \(app.buttons.allElementsBoundByIndex.prefix(12).map(\.label))")
            }
        }
        if let position = env["TJ_POSITION"] {
            app.terminate()
            app.launchArguments = ["-serverURL", server, "-apiKey", key, "-open", "position/\(position)"]
            app.launch()
            let edit = app.buttons.matching(NSPredicate(format: "label CONTAINS 'the plan' OR label CONTAINS 'stop and target'")).firstMatch
            _ = edit.waitForExistence(timeout: 20)
            shot("position")
            if edit.exists {
                edit.tap()
                sleep(3)
                shot("position-plan")
            } else {
                XCTFail("no plan button; buttons: \(app.buttons.allElementsBoundByIndex.prefix(12).map(\.label))")
            }
        }
        app.terminate()
        app.launchArguments = ["-serverURL", server, "-apiKey", key, "-open", "plan"]
        app.launch()
        let write = app.buttons.matching(NSPredicate(format: "label == 'Write the plan' OR label == 'Edit'")).firstMatch
        if write.waitForExistence(timeout: 20) {
            write.tap()
            sleep(3)
            shot("plan-edit")
        }
    }

    private func shot(_ name: String) {
        let png = XCUIScreen.main.screenshot().pngRepresentation
        try? png.write(to: URL(fileURLWithPath: shots).appendingPathComponent(name + ".png"))
    }
}
