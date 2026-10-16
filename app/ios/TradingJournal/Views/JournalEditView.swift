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

    init(entry: JournalDetail, onSaved: @escaping (JournalDetail) -> Void = { _ in }, onDeleted: @escaping () -> Void = {}) {
        self.entry = entry
        self.onSaved = onSaved
        self.onDeleted = onDeleted
        _draft = State(initialValue: JournalUpdate(entry))
    }

    private var dirty: Bool { draft != JournalUpdate(entry) }

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
                Button("Discard", role: .destructive) { dismiss() }
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
                labels = try? await l
                strategies = (try? await s) ?? []
            }
        }
    }

    private func save() async {
        busy = true
        defer { busy = false }
        do {
            let saved = try await client.saveJournal(entry.id, draft)
            error = nil
            onSaved(saved)
            dismiss()
        } catch Client.JournalSaveError.reviewPending {
            reviewPending = true
        } catch Client.JournalSaveError.stale(let current) {
            stale = current
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func delete() async {
        busy = true
        defer { busy = false }
        do {
            try await client.deleteJournal(entry.id)
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
    @State private var error: String?

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
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextEditor(text: $text).font(Brand.text(15, relativeTo: .body)).frame(minHeight: 180).accessibilityLabel("Note")
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
                    Button(busy ? "Saving…" : "Save") { Task { await create() } }.disabled(busy || text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
            .task {
                async let l = client.labels()
                async let s = client.strategies()
                labels = try? await l
                strategies = (try? await s) ?? []
            }
        }
    }

    private func create() async {
        busy = true
        defer { busy = false }
        do {
            var make = JournalCreate(scope: scope, body: text, tag: tag, date: date, tradeId: tradeId)
            make.strategyId = strategyId
            make.mistakes = mistakes
            make.emotions = emotions
            let e = try await client.createJournal(make)
            onCreated(e)
            dismiss()
        } catch Client.JournalSaveError.exists(let id) {
            onExists(id)
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
