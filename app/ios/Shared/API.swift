// The journal's HTTP API (API.md) as the app and its extensions call it: one bearer key, JSON in
// and out, the server's {"error": …} turned into a TJError. Every action the app takes is a plain
// request here; the event stream (Client) is only a faster way to learn that something changed.
import Foundation

enum TJError: LocalizedError, Equatable {
    case notConfigured
    case http(Int, String)
    case unauthorized
    case decoding(String)
    /// A 409 with the server's reason ("stale", "review-pending", or none) and its whole body.
    case conflict(reason: String, message: String, body: Data)

    var errorDescription: String? {
        switch self {
        case .notConfigured: "pair this phone with the journal first"
        case .unauthorized: "the journal no longer accepts this phone's key: pair again"
        case .http(let code, let msg):
            if isMissingRoute { "the journal is older than this app: update the server" } else { msg.isEmpty ? "the journal answered \(code)" : msg }
        case .decoding(let what): "unexpected answer from the journal: \(what)"
        case .conflict(_, let msg, _): msg.isEmpty ? "the journal refused: it changed meanwhile" : msg
        }
    }

    /// A call the server has no route for (its mux answers without a JSON error): an older server.
    var isMissingRoute: Bool {
        if case .http(let code, let msg) = self { return (code == 404 || code == 405) && msg.isEmpty }
        return false
    }
}

/// The server's error body, and the two 409 shapes a journal edit can meet.
struct APIErrorBody: Decodable, Sendable {
    var error: String?
    var reason: String?
}

struct TJAPI: Sendable {
    let base: URL
    let key: String

    init?(serverURL: String, key: String) {
        guard let base = ServerURL.base(serverURL), !key.isEmpty else { return nil }
        self.base = base
        self.key = key
    }

    /// What the app saved in the shared keychain; nil before pairing (or for a build whose
    /// signing has no shared access group).
    static func fromKeychain() -> TJAPI? {
        TJAPI(serverURL: Keychain.get("serverURL") ?? "", key: Keychain.get("apiKey") ?? "")
    }

    /// The absolute URL of an API path ("/today") or of a server-relative href the API hands
    /// out ("/api/v1/attachments/3", "/api/v1/trades/5/chart.png").
    func url(_ path: String, query: [URLQueryItem] = []) -> URL {
        var url = path.hasPrefix("/api/") ? base.appending(path: path) : base.appending(path: "/api/v1" + path)
        if !query.isEmpty { url.append(queryItems: query) }
        return url
    }

    func request(_ method: String, _ path: String, query: [URLQueryItem] = [], body: Data? = nil, contentType: String = "application/json", timeout: TimeInterval = 15) -> URLRequest {
        var req = URLRequest(url: url(path, query: query), timeoutInterval: timeout)
        req.httpMethod = method
        req.setValue("Bearer \(key)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        if let body {
            req.setValue(contentType, forHTTPHeaderField: "Content-Type")
            req.httpBody = body
        }
        return req
    }

    /// Sends a request and returns the body of a 2xx answer; anything else is a TJError.
    @discardableResult
    func send(_ method: String, _ path: String, query: [URLQueryItem] = [], body: Data? = nil, contentType: String = "application/json",
              session: URLSession = .shared) async throws -> Data {
        let (data, resp) = try await session.data(for: request(method, path, query: query, body: body, contentType: contentType))
        return try Self.check(data, resp)
    }

    static func check(_ data: Data, _ resp: URLResponse) throws -> Data {
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 401 { throw TJError.unauthorized }
        if code == 409 {
            let b = try? JSONDecoder().decode(APIErrorBody.self, from: data)
            throw TJError.conflict(reason: b?.reason ?? "", message: b?.error ?? "", body: data)
        }
        if code < 200 || code >= 300 {
            let msg = (try? JSONDecoder().decode(APIErrorBody.self, from: data))?.error ?? ""
            throw TJError.http(code, msg)
        }
        return data
    }

    /// GET and decode.
    func get<T: Decodable & Sendable>(_ type: T.Type, _ path: String, query: [URLQueryItem] = [], session: URLSession = .shared) async throws -> T {
        let data = try await send("GET", path, query: query, session: session)
        do {
            return try Wire.decoder.decode(T.self, from: data)
        } catch {
            throw TJError.decoding(String(describing: error).prefix(200).description)
        }
    }

    /// Sends JSON and decodes the answer.
    func send<B: Encodable & Sendable, T: Decodable & Sendable>(_ method: String, _ path: String, json body: B, as type: T.Type, session: URLSession = .shared) async throws -> T {
        let data = try await send(method, path, body: Wire.encoder.encode(body), session: session)
        do {
            return try Wire.decoder.decode(T.self, from: data)
        } catch {
            throw TJError.decoding(String(describing: error).prefix(200).description)
        }
    }

    /// Uploads one file as multipart/form-data (field "file") and decodes the answer.
    func upload<T: Decodable & Sendable>(_ path: String, file: Data, name: String, mimeType: String, as type: T.Type, session: URLSession = .shared) async throws -> T {
        let boundary = "tj-" + UUID().uuidString
        let body = Multipart.body(boundary: boundary, field: "file", filename: name, mimeType: mimeType, data: file)
        let data = try await send("POST", path, body: body, contentType: "multipart/form-data; boundary=\(boundary)", session: session)
        do {
            return try Wire.decoder.decode(T.self, from: data)
        } catch {
            throw TJError.decoding(String(describing: error).prefix(200).description)
        }
    }

    /// An image the API serves (a chart PNG, an attachment), with the key.
    func image(_ href: String, session: URLSession = .shared) async throws -> Data {
        var req = request("GET", href, timeout: 20)
        req.setValue("image/png, image/*", forHTTPHeaderField: "Accept")
        let (data, resp) = try await session.data(for: req)
        return try Self.check(data, resp)
    }

    /// Exchanges a pairing code for a device key: the one call made without a key.
    static func pair(serverURL: String, code: String, deviceName: String, session: URLSession = .shared) async throws -> PairResponse {
        guard let base = ServerURL.base(serverURL) else { throw TJError.notConfigured }
        var req = URLRequest(url: base.appending(path: "/api/v1/pair"), timeoutInterval: 15)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try Wire.encoder.encode(PairRequest(code: code, deviceName: deviceName))
        let (data, resp) = try await session.data(for: req)
        let body = try check(data, resp)
        return try Wire.decoder.decode(PairResponse.self, from: body)
    }
}

/// One file in a multipart/form-data body, as Go's multipart reader expects it.
enum Multipart {
    nonisolated static func body(boundary: String, field: String, filename: String, mimeType: String, data: Data) -> Data {
        var b = Data()
        b.append(Data("--\(boundary)\r\n".utf8))
        b.append(Data("Content-Disposition: form-data; name=\"\(field)\"; filename=\"\(filename.replacingOccurrences(of: "\"", with: ""))\"\r\n".utf8))
        b.append(Data("Content-Type: \(mimeType)\r\n\r\n".utf8))
        b.append(data)
        b.append(Data("\r\n--\(boundary)--\r\n".utf8))
        return b
    }
}

/// The server address as the user types it, or as a pairing QR carries it.
enum ServerURL {
    /// "journal.example.net" → https://journal.example.net; a path or a trailing slash is dropped;
    /// http:// is kept (a home-network address). Nil when nothing usable is there.
    static func base(_ s: String) -> URL? {
        var t = s.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { return nil }
        if !t.contains("://") { t = "https://" + t }
        guard var c = URLComponents(string: t), let host = c.host, !host.isEmpty else { return nil }
        c.path = ""
        c.query = nil
        c.fragment = nil
        if c.scheme != "http" && c.scheme != "https" { c.scheme = "https" }
        return c.url
    }
}
