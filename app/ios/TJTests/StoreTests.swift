// The store of the journal's answers: keys, the round trip, eviction, and the client reading
// through it when the journal is out of reach. The transport is a URLProtocol the test answers.
import Foundation
import Testing
@testable import TradingJournal

struct StoreTests {
    private func temp() -> URL {
        FileManager.default.temporaryDirectory.appending(path: "tj-store-\(UUID().uuidString)", directoryHint: .isDirectory)
    }

    @Test func keyIsThePathAndTheQueryInOrderWithoutRefresh() {
        #expect(Store.key("/today") == "/today")
        #expect(Store.key("/today", query: [URLQueryItem(name: "refresh", value: "1")]) == "/today", "refresh asks for fresh numbers, not for something else")
        let a = Store.key("/trades", query: [URLQueryItem(name: "page", value: "1"), URLQueryItem(name: "from", value: "2026-10-01")])
        let b = Store.key("/trades", query: [URLQueryItem(name: "from", value: "2026-10-01"), URLQueryItem(name: "page", value: "1")])
        #expect(a == b && a == "/trades?from=2026-10-01&page=1")
        #expect(Store.scope(serverURL: "https://j.example.net", key: "tj_a") != Store.scope(serverURL: "https://j.example.net", key: "tj_b"))
        #expect(Store.hash("x").count == 32)
    }

    @Test func keepsAnAnswerWithItsTimeAndForgetsOnWipe() async throws {
        let dir = temp()
        defer { try? FileManager.default.removeItem(at: dir) }
        let s = Store(dir: dir, cap: 1 << 20, maxAge: 30 * 86400)
        let none = await s.get("/today")
        #expect(none == nil)
        let at = Date(timeIntervalSince1970: 1_792_000_000)
        await s.put("/today", Data("{\"v\":1}".utf8), at: at)
        let (data, when) = try #require(await s.get("/today"))
        #expect(String(decoding: data, as: UTF8.self) == "{\"v\":1}" && when == at)
        let bytes = await s.bytes, count = await s.count
        #expect(bytes == 7 && count == 1)

        // a second store on the same directory reads the index back
        let again = Store(dir: dir, cap: 1 << 20, maxAge: 30 * 86400)
        let reread = await again.get("/today")
        #expect(reread?.1 == at)

        await s.wipe()
        let gone = await s.get("/today"), left = await s.bytes
        #expect(gone == nil && left == 0)
    }

    @Test func theLeastRecentlyUsedLeavesOverTheCap() async throws {
        let dir = temp()
        defer { try? FileManager.default.removeItem(at: dir) }
        let s = Store(dir: dir, cap: 12)
        await s.put("a", Data("aaaaa".utf8), at: Date(timeIntervalSinceNow: -30))
        await s.put("b", Data("bbbbb".utf8), at: Date(timeIntervalSinceNow: -20))
        _ = await s.get("a") // a is now the one used last
        await s.put("c", Data("ccccc".utf8))
        let b = await s.get("b"), a = await s.get("a"), c = await s.get("c"), total = await s.bytes
        #expect(b == nil, "b was used least recently")
        #expect(a != nil && c != nil)
        #expect(total == 10)
    }

    @Test func sweepDropsWhatIsTooOld() async throws {
        let dir = temp()
        defer { try? FileManager.default.removeItem(at: dir) }
        let s = Store(dir: dir, cap: 1 << 20, maxAge: 30 * 86400)
        await s.put("old", Data("o".utf8), at: Date(timeIntervalSinceNow: -40 * 86400))
        await s.put("new", Data("n".utf8))
        await s.sweep()
        let old = await s.get("old"), new = await s.get("new")
        #expect(old == nil && new != nil)
    }

    @Test func draftsRoundTrip() throws {
        let d = try #require(UserDefaults(suiteName: "tj-tests-drafts-\(UUID().uuidString)"))
        let key = Drafts.key(entry: 7)
        #expect(Drafts.load(JournalUpdate.self, key, defaults: d) == nil)
        let e = try Fixtures.load(JournalDetail.self, "journal_detail")
        var u = JournalUpdate(e)
        u.body = "half a thought"
        u.mistakes = ["chased"]
        Drafts.save(u, key, defaults: d)
        let kept = try #require(Drafts.load(JournalUpdate.self, key, defaults: d))
        #expect(kept.value == u && abs(kept.at.timeIntervalSinceNow) < 5)
        Drafts.clear(key, defaults: d)
        #expect(Drafts.load(JournalUpdate.self, key, defaults: d) == nil)

        var make = JournalCreate(scope: .trade, body: "x", tag: .bad, tradeId: 5)
        make.emotions = ["rushed"]
        let newKey = Drafts.key(new: .trade, date: nil, tradeId: 5)
        Drafts.save(make, newKey, defaults: d)
        #expect(Drafts.load(JournalCreate.self, newKey, defaults: d)?.value == make)
        #expect(Drafts.key(new: .day, date: "2026-10-16", tradeId: nil) != newKey)
    }

    @Test func loadedReadsThroughToItsValue() {
        let l = Loaded(value: [1, 2, 3], asOf: nil)
        #expect(l.count == 3 && !l.isStale)
        let m = l.map(\.count)
        #expect(m.value == 3 && m.asOf == nil)
        let stale = Loaded(value: "x", asOf: .now)
        #expect(stale.isStale)
    }
}

/// The transport under test: every request is answered by `handler`.
final class FakeTransport: URLProtocol {
    nonisolated(unsafe) static var handler: (@Sendable (URLRequest) -> Result<(Int, Data), URLError>)?
    nonisolated(unsafe) static var requests = 0

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.requests += 1
        guard let handler = Self.handler, let url = request.url else {
            client?.urlProtocol(self, didFailWithError: URLError(.badURL))
            return
        }
        switch handler(request) {
        case .success(let (code, data)):
            let resp = HTTPURLResponse(url: url, statusCode: code, httpVersion: nil, headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        case .failure(let error):
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

@MainActor
@Suite(.serialized) // one transport
struct ClientStoreTests {
    /// A client on a test pairing and a store of its own; the settings are put back after.
    private func client(_ body: (Client) async throws -> Void) async throws {
        let root = FileManager.default.temporaryDirectory.appending(path: "tj-client-\(UUID().uuidString)", directoryHint: .isDirectory)
        defer { try? FileManager.default.removeItem(at: root) }
        let s = Settings()
        let url = s.serverURL, key = s.apiKey
        defer { s.serverURL = url; s.apiKey = key }
        s.serverURL = "http://journal.test"
        s.apiKey = "tj_test"
        FakeTransport.requests = 0
        try await body(Client(settings: s, storeRoot: root, protocolClasses: [FakeTransport.self]))
    }

    @Test func aLiveAnswerIsKeptAndStandsInWhenTheJournalIsOutOfReach() async throws {
        try await client { c in
            let trades = try Fixtures.data("trades")
            FakeTransport.handler = { _ in .success((200, trades)) }
            let live = try await c.trades(.init())
            #expect(!live.isStale && !live.items.isEmpty)

            FakeTransport.handler = { _ in .failure(URLError(.notConnectedToInternet)) }
            let kept = try await c.trades(.init())
            #expect(kept.isStale && kept.items.count == live.items.count)
            #expect(kept.asOf.map { abs($0.timeIntervalSinceNow) < 10 } == true)

            // another page was never kept: the loss of reach is the error
            await #expect(throws: URLError.self) { _ = try await c.trades(.init(page: 2)) }

            // an answer from the journal is not a loss of reach, whatever it says
            FakeTransport.handler = { _ in .success((404, Data(#"{"error":"nope"}"#.utf8))) }
            await #expect(throws: TJError.http(404, "nope")) { _ = try await c.trades(.init()) }

            // a gateway with no journal behind it is
            FakeTransport.handler = { _ in .success((502, Data())) }
            let gateway = try await c.trades(.init())
            #expect(gateway.isStale)
        }
    }

    @Test func todayIsKeptAndReadBackAsStale() async throws {
        try await client { c in
            let today = try Fixtures.data("today")
            FakeTransport.handler = { _ in .success((200, today)) }
            await c.refreshToday()
            #expect(c.today != nil && c.todayAsOf == nil)

            FakeTransport.handler = { _ in .failure(URLError(.cannotConnectToHost)) }
            await c.refreshToday()
            #expect(c.today != nil && c.todayAsOf != nil, "the kept day stands in, marked as kept")
        }
    }

    @Test func imagesAreKept() async throws {
        try await client { c in
            let png = Data([0x89, 0x50, 0x4E, 0x47])
            FakeTransport.handler = { _ in .success((200, png)) }
            let first = try await c.image("/api/v1/trades/5/chart.png")
            #expect(first == png)
            let after = FakeTransport.requests
            let second = try await c.image("/api/v1/trades/5/chart.png")
            #expect(second == png)
            #expect(FakeTransport.requests == after, "a fresh image is not fetched again")
            FakeTransport.handler = { _ in .failure(URLError(.notConnectedToInternet)) }
            let offline = try await c.image("/api/v1/trades/5/chart.png")
            #expect(offline == png)
        }
    }

    @Test func signingOutForgetsTheStore() async throws {
        try await client { c in
            let trades = try Fixtures.data("trades")
            FakeTransport.handler = { _ in .success((200, trades)) }
            _ = try await c.trades(.init())
            let before = await c.storedBytes()
            #expect(before > 0)
            await c.clearStore()
            let after = await c.storedBytes()
            #expect(after == 0)
        }
    }
}
