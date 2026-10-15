// Settings → Notifications: which pushes this phone gets, with which sound, whether they break
// through Focus, and the Live Activity choices. The server holds a copy and decides before every
// push (iOS cannot drop a push once it arrives); the badge still counts everything unseen.
import AudioToolbox
import SwiftUI

struct NotificationSettingsView: View {
    @Environment(Client.self) private var client
    @Environment(Settings.self) private var settings
    @State private var notifications = Notifications.shared

    private var prefs: DevicePrefs { settings.notificationPrefs }

    var body: some View {
        Form {
            ForEach(PushKind.allCases) { kind in
                Section {
                    Toggle(kind.title, isOn: Binding(get: { prefs.isOn(kind) }, set: { on in notifications.updatePrefs { $0.setOn(kind, on) } }))
                        .tint(Brand.accent)
                    if prefs.isOn(kind) {
                        Picker("Sound", selection: Binding(get: { prefs.sound(kind) }, set: { s in
                            notifications.updatePrefs { $0.setSound(kind, s) }
                            SoundPreview.play(s, kind: kind)
                        })) {
                            ForEach(PushSound.allCases) { Text($0.title).tag($0) }
                        }
                        Toggle("Break through Focus", isOn: Binding(get: { prefs.timeSensitive(kind) }, set: { on in notifications.updatePrefs { $0.setTimeSensitive(kind, on) } }))
                            .tint(Brand.accent)
                        if kind == .tradeClosed {
                            Toggle("Only during regular hours", isOn: Binding(get: { prefs.quietOutsideRth == true }, set: { on in notifications.updatePrefs { $0.quietOutsideRth = on ? true : nil } }))
                                .tint(Brand.accent)
                        }
                    }
                } footer: {
                    Text(kind.detail)
                }
            }
            Section {
                Toggle("Session in the Dynamic Island and on the Lock Screen", isOn: Binding(get: { prefs.showsActivity }, set: { on in notifications.updatePrefs { $0.noActivity = on ? nil : true } }))
                    .tint(Brand.accent)
                if prefs.showsActivity {
                    Toggle("Closed trades as notifications too", isOn: Binding(get: { prefs.finishedAsNotification == true }, set: { on in notifications.updatePrefs { $0.finishedAsNotification = on ? true : nil } }))
                        .tint(Brand.accent)
                }
            } header: {
                Text("Live Activity")
            } footer: {
                Text("The Session activity starts on the first fill of the day and ends when the day is flat and quiet. While it shows, a closed trade lights it up with its sound instead of a separate notification; nudges are always notifications. The activity itself arrives with the next build of the app.")
            }
            if let e = notifications.prefsError {
                Section {
                    Text("Not on the journal yet: \(e)").font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.warnText)
                    Button("Try again") { Task { await notifications.syncPrefs() } }
                }
            }
        }
        .navigationTitle("Notifications")
        .navigationBarTitleDisplayMode(.inline)
    }
}

/// Plays the sound a choice stands for, so the picker can be heard.
enum SoundPreview {
    @MainActor private static var ids: [String: SystemSoundID] = [:]

    @MainActor static func play(_ s: PushSound, kind: PushKind) {
        switch s {
        case .none:
            return
        case .system:
            AudioServicesPlaySystemSound(1007) // the tri-tone a notification makes by default
        case .tj:
            let name = kind.appSound
            if ids[name] == nil, let url = Bundle.main.url(forResource: name, withExtension: "caf") {
                var id: SystemSoundID = 0
                if AudioServicesCreateSystemSoundID(url as CFURL, &id) == noErr { ids[name] = id }
            }
            if let id = ids[name] { AudioServicesPlaySystemSound(id) }
        }
    }
}
