// The day plan before the open, and the plan on an open position with the risk mirrored live.
import SwiftUI

struct PlanEditView: View {
    let day: String
    let plan: DayPlan?
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var draft: DayPlanSave
    @State private var maxTrades = ""
    @State private var maxLoss = ""
    @State private var busy = false
    @State private var error: String?

    init(day: String, plan: DayPlan?) {
        self.day = day
        self.plan = plan
        _draft = State(initialValue: DayPlanSave(day: day, plan: plan))
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker("Bias", selection: $draft.bias) {
                        Text("none").tag(Bias.none)
                        Text("long").tag(Bias.long)
                        Text("short").tag(Bias.short)
                        Text("neutral").tag(Bias.neutral)
                        Text("flat").tag(Bias.flat)
                    }
                    .pickerStyle(.segmented)
                    LabeledContent("Max trades") {
                        TextField("none", text: $maxTrades).keyboardType(.numberPad).multilineTextAlignment(.trailing).font(Brand.mono(15))
                    }
                    LabeledContent("Stop time") {
                        TextField("HH:MM", text: $draft.stopTime).keyboardType(.numbersAndPunctuation).multilineTextAlignment(.trailing).font(Brand.mono(15))
                    }
                    LabeledContent("Max loss") {
                        TextField("none", text: $maxLoss).keyboardType(.decimalPad).multilineTextAlignment(.trailing).font(Brand.mono(15))
                    }
                } header: {
                    Text("Rules for \(Formatting.dayLabel(day, tz: client.tz))")
                } footer: {
                    Text("The guardrail measures the day against these: a trade over the max, a fill after the stop time, a loss past the budget. Empty clears a rule.")
                }
                Section("Levels") {
                    TextEditor(text: $draft.levels).font(Brand.mono(13)).frame(minHeight: 70).accessibilityLabel("Levels")
                }
                Section("Notes") {
                    TextEditor(text: $draft.notes).font(Brand.text(15, relativeTo: .body)).frame(minHeight: 100).accessibilityLabel("Notes")
                }
                if let error { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) }
            }
            .navigationTitle(plan == nil ? "Write the plan" : "Edit the plan")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) { Button(busy ? "Saving…" : "Save") { Task { await save() } }.disabled(busy) }
            }
            .onAppear {
                maxTrades = draft.maxTrades > 0 ? String(draft.maxTrades) : ""
                maxLoss = draft.maxLoss > 0 ? String(format: "%g", draft.maxLoss) : ""
            }
        }
    }

    private func save() async {
        draft.maxTrades = Int(maxTrades.trimmingCharacters(in: .whitespaces)) ?? 0
        draft.maxLoss = Double(maxLoss.replacingOccurrences(of: ",", with: ".").trimmingCharacters(in: .whitespaces)) ?? 0
        busy = true
        defer { busy = false }
        do {
            _ = try await client.savePlan(draft)
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// The numbers behind the position plan editor, pure for the tests.
enum PlanMath {
    /// The money at risk at the position's size, nil without a stop.
    nonisolated static func risk(stop: Double?, position p: Position) -> Double? {
        guard let stop, let avg = p.avgEntry else { return nil }
        return Formatting.risk(avgEntry: avg, stop: stop, qty: p.qty, pointValue: p.pointValue)
    }

    /// Reward over risk, nil without both levels or with a stop at the entry.
    nonisolated static func rewardRisk(stop: Double?, target: Double?, avgEntry: Double?) -> Double? {
        guard let stop, let target, let avg = avgEntry, avg != stop else { return nil }
        return abs((target - avg) / (avg - stop))
    }

    /// A stop on the wrong side of the entry for the direction.
    nonisolated static func stopIsWrongSide(stop: Double?, position p: Position) -> Bool {
        guard let stop, let avg = p.avgEntry else { return false }
        return p.direction == .long ? stop >= avg : p.direction == .short ? stop <= avg : false
    }

    nonisolated static func parse(_ s: String) -> Double? {
        let t = s.replacingOccurrences(of: ",", with: ".").trimmingCharacters(in: .whitespaces)
        return t.isEmpty ? nil : Double(t)
    }
}

struct PositionPlanEditView: View {
    let position: Position
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var stop = ""
    @State private var target = ""
    @State private var thesis = ""
    @State private var allLegs = true
    @State private var busy = false
    @State private var error: String?

    private var stopValue: Double? { PlanMath.parse(stop) }
    private var targetValue: Double? { PlanMath.parse(target) }

    var body: some View {
        let p = position
        NavigationStack {
            Form {
                Section {
                    LabeledContent("Stop") {
                        TextField("price", text: $stop).keyboardType(.decimalPad).multilineTextAlignment(.trailing).font(Brand.mono(15))
                    }
                    LabeledContent("Target") {
                        TextField("price", text: $target).keyboardType(.decimalPad).multilineTextAlignment(.trailing).font(Brand.mono(15))
                    }
                    LabeledContent("Risk") {
                        Text(Formatting.money(PlanMath.risk(stop: stopValue, position: p).map { -$0 })).font(Brand.mono(15)).foregroundStyle(stopValue == nil ? Brand.fgSubtle : Brand.negText)
                    }
                    LabeledContent("R:R") {
                        Text(PlanMath.rewardRisk(stop: stopValue, target: targetValue, avgEntry: p.avgEntry).map { String(format: "%.1f", $0) } ?? "—").font(Brand.mono(15))
                    }
                    if PlanMath.stopIsWrongSide(stop: stopValue, position: p) {
                        Text("That stop is on the wrong side of a \(p.direction.label) entry at \(Formatting.price(p.avgEntry, tickSize: p.tickSize)).").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.warnText)
                    }
                } header: {
                    Text("\(p.symbol) \(p.headline)")
                } footer: {
                    Text("Risk is |entry − stop| × \(Formatting.qty(p.pointValue)) a point × \(Formatting.qty(p.qty)). The server snaps the stop to the \(Formatting.price(p.tickSize, tickSize: p.tickSize)) tick.")
                }
                Section("Thesis") {
                    TextEditor(text: $thesis).font(Brand.text(15, relativeTo: .body)).frame(minHeight: 80).accessibilityLabel("Thesis")
                }
                if p.setLegs.count > 1 {
                    Section {
                        Toggle("Write it on every leg", isOn: $allLegs).tint(Brand.accent)
                    } footer: {
                        Text("A copied decision on \(p.setLegs.count) accounts: the same plan on each, or this leg only.")
                    }
                }
                if p.hasPlan {
                    Section {
                        Button("Clear the plan", role: .destructive) { stop = ""; target = ""; Task { await save() } }
                    }
                }
                if let error { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) }
            }
            .navigationTitle(p.hasPlan ? "Edit the plan" : "Set the plan")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(busy ? "Saving…" : "Save") { Task { await save() } }
                        .disabled(busy || (stopValue == nil && !stop.isEmpty) || (targetValue == nil && !target.isEmpty))
                }
            }
            .onAppear {
                stop = p.stop.map { Formatting.price($0, tickSize: p.tickSize) } ?? ""
                target = p.target.map { Formatting.price($0, tickSize: p.tickSize) } ?? ""
                thesis = p.thesisText
            }
        }
    }

    private func save() async {
        busy = true
        defer { busy = false }
        do {
            _ = try await client.savePositionPlan(account: position.accountId, symbol: position.symbol,
                                                  PositionPlanSave(stop: stopValue, risk: nil, target: targetValue, thesis: thesis, allLegs: allLegs))
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
