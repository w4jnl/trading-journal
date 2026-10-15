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
    }

    private func shot(_ name: String) {
        let png = XCUIScreen.main.screenshot().pngRepresentation
        try? png.write(to: URL(fileURLWithPath: shots).appendingPathComponent(name + ".png"))
    }
}
