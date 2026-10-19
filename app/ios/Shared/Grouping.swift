// The "Trades grouped by set" switch, mirrored into the App Group so the widget extension can
// follow it: the Session activity's faces and the Today widgets fold a copied decision's legs
// into one row and count decisions instead of trades when it is on. The fold itself lives here
// too, on the positions the server lists, so Today's card and the widgets' cache agree.
import Foundation

enum Grouping {
    static let key = "tradesBySet"

    /// Whether the trader groups by set, as the app last saved it; false before the first save.
    nonisolated static func bySet(defaults: UserDefaults? = UserDefaults(suiteName: TodayCache.suite)) -> Bool {
        defaults?.object(forKey: key) as? Bool ?? false
    }

    nonisolated static func save(_ bySet: Bool, defaults: UserDefaults? = UserDefaults(suiteName: TodayCache.suite)) {
        defaults?.set(bySet, forKey: key)
    }
}

extension Position {
    /// The decision this leg belongs to: its set's accounts (this one included) and the contract;
    /// the leg itself when it is on its own. Matches push.SessionPosition.SetKey on the server.
    var setKey: String {
        let ids = setLegs.isEmpty ? [accountId] : (setLegs + [accountId]).sorted()
        return ids.map(String.init).joined(separator: ",") + ":" + symbol
    }
}

extension [Position] {
    /// The distinct decisions among these open positions.
    var openSets: Int { Set(map(\.setKey)).count }

    /// One position per decision: the legs' size and unrealised summed, the first leg's prices
    /// and plan, the stop distance of the leg nearest its stop, through the stop if any leg is,
    /// the account names joined. Order follows the first leg of each.
    func foldedBySet() -> [Position] {
        var out: [Position] = []
        var index: [String: Int] = [:]
        for p in self {
            let k = p.setKey
            guard p.setLegs.count > 0, let i = index[k] else {
                index[k] = out.count
                out.append(p)
                continue
            }
            out[i].qty += p.qty
            if let u = p.unrealized { out[i].unrealized = (out[i].unrealized ?? 0) + u }
            out[i].stale = out[i].stale || p.stale
            out[i].beyondStop = out[i].beyondStop || p.beyondStop
            if let t = p.ticksToStop, out[i].ticksToStop.map({ abs(t) < abs($0) }) ?? true { out[i].ticksToStop = t }
            out[i].account += ", " + p.account
        }
        return out
    }
}
