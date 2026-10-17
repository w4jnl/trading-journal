// Writing in the journal from the phone: edit an entry whole (the way a page save does), append
// a dated note, or create the first note on a trade or a day. A save that meets an AI review in
// progress offers to append instead; one that meets a changed entry offers to reload or overwrite.
import PhotosUI
import SwiftUI

struct JournalEditView: View {
    let entry: JournalDetail
    var onSaved: (JournalDetail) -> Void = { _ in }
    var onDeleted: () -> Void = {}
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var draft: JournalUpdate
    @State private var labels: LabelVocabulary?
    @State private var strategies: [Strategy] = []
    @State private var photo: PhotosPickerItem?
    @State private var busy = false
    @State private var error: String?
    @State private var reviewPending = false
    @State private var stale: JournalDetail?
    @State private var confirmDelete = false
    @State private var confirmDiscard = false
    @State private var append = false
    /// When the draft opened from one kept on this phone: the time it was kept.
    @State private var keptAt: Date?

    init(entry: JournalDetail, onSaved: @escaping (JournalDetail) -> Void = { _ in }, onDeleted: @escaping () -> Void = {}) {
        self.entry = entry
        self.onSaved = onSaved
        self.onDeleted = onDeleted
        // A save that met no answer left its words here; the editor opens on them.
        if let kept = Drafts.load(JournalUpdate.self, Drafts.key(entry: entry.id)) {
            _draft = State(initialValue: kept.value)
            _keptAt = State(initialValue: kept.at)
        } else {
            _draft = State(initialValue: JournalUpdate(entry))
        }
    }

    private var dirty: Bool { draft != JournalUpdate(entry) }
    private var draftKey: String { Drafts.key(entry: entry.id) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextEditor(text: $draft.body)
                        .font(Brand.text(15, relativeTo: .body))
                        .frame(minHeight: 180)
                        .accessibilityLabel("Note")
                    HStack {
                        PhotosPicker(selection: $photo, matching: .images) { Label("Add a photo", systemImage: "photo") }
                            .onChange(of: photo) { _, item in if let item { Task { await attach(item) } } }
                        Spacer()
                        Text("Markdown").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                    }
                } header: {
                    Text(Formatting.dayLabel(entry.date, tz: client.tz, style: .long))
                } footer: {
                    if let keptAt {
                        HStack {
                            Text("A draft kept on this phone at \(StaleBanner.when(keptAt, tz: client.tz)); not saved to the journal yet.")
                            Button("Discard it") { draft = JournalUpdate(entry); Drafts.clear(draftKey); self.keptAt = nil }
                        }
                    }
                }
                Section("Tag") {
                    Picker("Tag", selection: $draft.tag) {
                        Text("none").tag(JournalTag?.none)
                        Text("good").tag(JournalTag?.some(.good))
                        Text("neutral").tag(JournalTag?.some(.neutral))
                        Text("bad").tag(JournalTag?.some(.bad))
                    }
                    .pickerStyle(.segmented)
                    if entry.scope == .day {
                        Picker("Mood", selection: $draft.mood) {
                            Text("—").tag(Int?.none)
                            ForEach(1...5, id: \.self) { Text("\($0)").tag(Int?.some($0)) }
                        }
                        .pickerStyle(.segmented)
                    }
                    Picker("Strategy", selection: $draft.strategyId) {
                        Text("none").tag(Int?.none)
                        ForEach(strategies) { Text($0.name).tag(Int?.some($0.id)) }
                    }
                }
                if let labels {
                    Section("Mistakes") { LabelChips(options: labels.mistakes, selected: $draft.mistakes, kind: .error) }
                    Section("Emotions") { LabelChips(options: labels.emotions, selected: $draft.emotions, kind: .info) }
                }
                if let c = entry.checklist, !c.items.isEmpty {
                    Section("Checklist") {
                        ForEach(c.items) { i in
                            Toggle(i.text, isOn: Binding(
                                get: { draft.checks.contains(i.id) },
                                set: { on in if on { draft.checks.append(i.id) } else { draft.checks.removeAll { $0 == i.id } } }
                            ))
                            .tint(Brand.accent)
                        }
                    }
                }
                Section {
                    Button("Delete this entry", role: .destructive) { confirmDelete = true }
                        .confirmationDialog("Delete the entry and its note?", isPresented: $confirmDelete, titleVisibility: .visible) {
                            Button("Delete", role: .destructive) { Task { await delete() } }
                        }
                }
                if let error { Section { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) } }
            }
            .navigationTitle("Edit note")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { if dirty { confirmDiscard = true } else { dismiss() } } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(busy ? "Saving…" : "Save") { Task { await save() } }
                        .disabled(busy || draft.body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
            .interactiveDismissDisabled(dirty)
            .confirmationDialog("Discard the changes?", isPresented: $confirmDiscard, titleVisibility: .visible) {
                Button("Discard", role: .destructive) { Drafts.clear(draftKey); dismiss() }
            }
            .alert("An AI review is writing this note", isPresented: $reviewPending) {
                Button("Append a note instead") { append = true }
                Button("Wait", role: .cancel) {}
            } message: {
                Text("The note is locked until the review lands. You can append your words under it now, or save again later.")
            }
            .alert("The entry changed meanwhile", isPresented: Binding(get: { stale != nil }, set: { if !$0 { stale = nil } })) {
                Button("Reload theirs") {
                    if let s = stale { draft = JournalUpdate(s); onSaved(s) }
                    stale = nil
                }
                Button("Overwrite", role: .destructive) {
                    if let s = stale { draft.updatedAt = s.updatedAt }
                    stale = nil
                    Task { await save() }
                }
                Button("Cancel", role: .cancel) { stale = nil }
            } message: {
                Text("The desk or an AI review saved this entry after you opened it. Reload to see theirs and lose your edits, or overwrite with yours.")
            }
            .sheet(isPresented: $append) {
                AppendNoteView(entryId: entry.id, initial: draft.body == entry.notes ? "" : draft.body) { dismiss() }
            }
            .task {
                async let l = client.labels()
                async let s = client.strategies()
                labels = (try? await l)?.value
                strategies = (try? await s)?.value ?? []
            }
        }
    }

    private func save() async {
        busy = true
        defer { busy = false }
        do {
            let saved = try await client.saveJournal(entry.id, draft)
            error = nil
            Drafts.clear(draftKey)
            onSaved(saved)
            dismiss()
        } catch Client.JournalSaveError.reviewPending {
            reviewPending = true
        } catch Client.JournalSaveError.stale(let current) {
            stale = current
        } catch {
            // The journal did not take it: the words stay on this phone for the next try.
            if Client.isReachability(error) {
                Drafts.save(draft, draftKey)
                keptAt = .now
                self.error = "The journal is out of reach. Your draft is kept on this phone; save again when it answers."
            } else {
                self.error = error.localizedDescription
            }
        }
    }

    private func delete() async {
        busy = true
        defer { busy = false }
        do {
            try await client.deleteJournal(entry.id)
            Drafts.clear(draftKey)
            onDeleted()
            dismiss()
        } catch Client.JournalSaveError.reviewPending {
            reviewPending = true
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func attach(_ item: PhotosPickerItem) async {
        busy = true
        defer { busy = false; photo = nil }
        do {
            guard let data = try await item.loadTransferable(type: Data.self) else { return }
            let (jpeg, name) = ImageShrink.jpeg(data, name: "photo-\(Int(Date().timeIntervalSince1970)).jpg")
            let up = try await client.upload(image: jpeg, name: name, mimeType: "image/jpeg")
            draft.body += (draft.body.hasSuffix("\n") || draft.body.isEmpty ? "" : "\n\n") + up.markdown + "\n"
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// A photo as the API takes it: JPEG, at most ~1600 px on the long side.
enum ImageShrink {
    nonisolated static func jpeg(_ data: Data, name: String, maxSide: CGFloat = 1600) -> (Data, String) {
        guard let img = UIImage(data: data) else { return (data, name) }
        let scale = min(1, maxSide / max(img.size.width, img.size.height))
        let size = CGSize(width: img.size.width * scale, height: img.size.height * scale)
        let r = UIGraphicsImageRenderer(size: size, format: { let f = UIGraphicsImageRendererFormat(); f.scale = 1; return f }())
        let out = r.image { _ in img.draw(in: CGRect(origin: .zero, size: size)) }
        return (out.jpegData(compressionQuality: 0.85) ?? data, name)
    }
}

/// Mistake and emotion labels as toggling chips.
struct LabelChips: View {
    let options: [LabelOption]
    @Binding var selected: [String]
    let kind: PillKind

    var body: some View {
        FlowLayout(spacing: 6) {
            ForEach(options) { o in
                let on = selected.contains(o.key)
                Button {
                    if on { selected.removeAll { $0 == o.key } } else { selected.append(o.key) }
                } label: {
                    Text(o.name)
                        .font(Brand.text(12, weight: .medium, relativeTo: .caption))
                        .foregroundStyle(on ? kind.fg : Brand.fgMuted)
                        .padding(.horizontal, 8).padding(.vertical, 4)
                        .background(on ? kind.bg : Brand.bgSunken)
                        .clipShape(Capsule())
                        .overlay(Capsule().strokeBorder(on ? kind.fg.opacity(0.4) : Brand.border, lineWidth: 1))
                }
                .buttonStyle(.plain)
                .accessibilityAddTraits(on ? .isSelected : [])
            }
        }
    }
}

/// Wraps its children into rows.
struct FlowLayout: Layout {
    var spacing: CGFloat = 6

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? 320
        var x: CGFloat = 0, y: CGFloat = 0, rowH: CGFloat = 0
        for s in subviews {
            let sz = s.sizeThatFits(.unspecified)
            if x + sz.width > width, x > 0 { x = 0; y += rowH + spacing; rowH = 0 }
            x += sz.width + spacing
            rowH = max(rowH, sz.height)
        }
        return CGSize(width: width, height: y + rowH)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var x = bounds.minX, y = bounds.minY, rowH: CGFloat = 0
        for s in subviews {
            let sz = s.sizeThatFits(.unspecified)
            if x + sz.width > bounds.maxX, x > bounds.minX { x = bounds.minX; y += rowH + spacing; rowH = 0 }
            s.place(at: CGPoint(x: x, y: y), proposal: .unspecified)
            x += sz.width + spacing
            rowH = max(rowH, sz.height)
        }
    }
}

/// Appends a dated note under an entry: the one write that never conflicts.
struct AppendNoteView: View {
    let entryId: Int
    var initial = ""
    var onDone: () -> Void = {}
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var text = ""
    @State private var busy = false
    @State private var analyzing = false
    @State private var error: String?

    private var blank: Bool { text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextEditor(text: $text).font(Brand.text(15, relativeTo: .body)).frame(minHeight: 160).accessibilityLabel("Note")
                } footer: {
                    Text("Added under a dated heading; nothing already written changes.")
                }
                if let error { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) }
            }
            .navigationTitle("Append a note")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(busy ? "Adding…" : "Add") { Task { await add() } }.disabled(busy || text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
            .onAppear { text = initial }
        }
    }

    private func add() async {
        busy = true
        defer { busy = false }
        do {
            try await client.appendNote(entryId, text)
            onDone()
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// The first note on a trade or a day: creates the entry; one that exists already is opened.
/// With AI ready it also starts the review from here: an empty sheet asks the server for the
/// note and its review in one go, a written note is saved first and reviewed as it stands.
struct NewEntryView: View {
    let scope: JournalScope
    var tradeId: Int?
    var date: String?
    var onCreated: (JournalDetail) -> Void = { _ in }
    var onExists: (Int) -> Void = { _ in }
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var text = ""
    @State private var tag: JournalTag?
    @State private var strategyId: Int?
    @State private var mistakes: [String] = []
    @State private var emotions: [String] = []
    @State private var labels: LabelVocabulary?
    @State private var strategies: [Strategy] = []
    @State private var busy = false
    @State private var analyzing = false
    @State private var error: String?
    /// When the fields opened from a draft kept on this phone: the time it was kept.
    @State private var keptAt: Date?

    private var blank: Bool { text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
    private var draftKey: String { Drafts.key(new: scope, date: date, tradeId: tradeId) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextEditor(text: $text).font(Brand.text(15, relativeTo: .body)).frame(minHeight: 140).accessibilityLabel("Note")
                } footer: {
                    if let keptAt {
                        HStack {
                            Text("A draft kept on this phone at \(StaleBanner.when(keptAt, tz: client.tz)); not saved to the journal yet.")
                            Button("Discard it") { text = ""; tag = nil; strategyId = nil; mistakes = []; emotions = []; Drafts.clear(draftKey); self.keptAt = nil }
                        }
                    }
                }
                if client.aiReady {
                    Section {
                        Button { Task { await analyze() } } label: {
                            Label(analyzing ? "Starting the review…" : blank ? "Analyze with the AI" : "Save and analyze", systemImage: "sparkles")
                                .font(Brand.text(15, weight: .medium, relativeTo: .body)).foregroundStyle(Brand.accentText)
                        }
                        .disabled(busy)
                        .accessibilityIdentifier("analyze-new")
                    } footer: {
                        Text(blank ? "The AI writes the first note and its review lands on it as suggestions." : "Your note is saved first; the review lands on it as suggestions.")
                    }
                }
                Section("Tag") {
                    Picker("Tag", selection: $tag) {
                        Text("none").tag(JournalTag?.none)
                        Text("good").tag(JournalTag?.some(.good))
                        Text("neutral").tag(JournalTag?.some(.neutral))
                        Text("bad").tag(JournalTag?.some(.bad))
                    }
                    .pickerStyle(.segmented)
                    if !strategies.isEmpty {
                        Picker("Strategy", selection: $strategyId) {
                            Text("none").tag(Int?.none)
                            ForEach(strategies) { Text($0.name).tag(Int?.some($0.id)) }
                        }
                    }
                }
                // The same labels the web app offers on every note, day or trade.
                if let labels {
                    Section("Mistakes") { LabelChips(options: labels.mistakes, selected: $mistakes, kind: .error) }
                    Section("Emotions") { LabelChips(options: labels.emotions, selected: $emotions, kind: .info) }
                }
                if let error { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) }
            }
            .navigationTitle(scope == .day ? "Day note" : "Trade note")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(busy && !analyzing ? "Saving…" : "Save") { Task { await create() } }.disabled(busy || blank)
                }
            }
            .task {
                if let kept = Drafts.load(JournalCreate.self, draftKey) {
                    text = kept.value.body
                    tag = kept.value.tag
                    strategyId = kept.value.strategyId
                    mistakes = kept.value.mistakes
                    emotions = kept.value.emotions
                    keptAt = kept.at
                }
                async let l = client.labels()
                async let s = client.strategies()
                labels = (try? await l)?.value
                strategies = (try? await s)?.value ?? []
            }
        }
    }

    private func create() async {
        busy = true
        defer { busy = false }
        do {
            let e = try await save()
            Drafts.clear(draftKey)
            onCreated(e)
            dismiss()
        } catch Client.JournalSaveError.exists(let id) {
            Drafts.clear(draftKey)
            onExists(id)
            dismiss()
        } catch {
            if Client.isReachability(error) {
                Drafts.save(makeBody(), draftKey)
                keptAt = .now
                self.error = "The journal is out of reach. Your draft is kept on this phone; save again when it answers."
            } else {
                self.error = error.localizedDescription
            }
        }
    }

    private func save() async throws -> JournalDetail {
        try await client.createJournal(makeBody())
    }

    /// The note as the fields stand.
    private func makeBody() -> JournalCreate {
        var make = JournalCreate(scope: scope, body: text, tag: tag, date: date, tradeId: tradeId)
        make.strategyId = strategyId
        make.mistakes = mistakes
        make.emotions = emotions
        return make
    }

    /// Starts the review and opens the note it lands on. Nothing written here: the server
    /// creates the note for the day or trade. Something written: saved, then reviewed by id.
    /// A note that exists already (written elsewhere meanwhile) is reviewed and opened.
    private func analyze() async {
        busy = true
        analyzing = true
        defer { busy = false; analyzing = false }
        let target: ReviewTarget = scope == .day ? .day(date ?? "") : .trade(tradeId ?? 0)
        do {
            let id: Int
            if blank {
                id = try await client.startReview(target)
            } else {
                do {
                    id = try await save().id
                } catch Client.JournalSaveError.exists(let existing) {
                    id = existing
                }
                _ = try await client.startReview(.entry(id))
            }
            onExists(id)
            dismiss()
        } catch Client.AIError.reviewPending(let id) {
            if let id { onExists(id); dismiss() } else { error = "A review of this note is already running." }
        } catch Client.AIError.off {
            error = "No AI provider: add your key in the web app's Settings > AI."
        } catch {
            self.error = error.localizedDescription
        }
    }
}
