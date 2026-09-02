// LaunchScreen.swift — the screen a launch can be told to open on.
//
// A phone UI can only be reviewed by running it: `screencapture` is blocked,
// ImageRenderer can't draw a ScrollView, and simctl has no tap. So the app takes
// the same kind of flag the Mac's snapshot tool takes — a RENDERING MODE, not a
// seed. It opens a screen the app can already reach; it invents no data, writes
// nothing to the server, and every row behind it came from a real fetch.
//
//   xcrun simctl launch booted com.anywatch.botnet.ios -openScreen project
//
// simctl passes `-key value` pairs into UserDefaults' *argument* domain, which
// lives only for that launch — nothing is written to the app's plist, so a
// normal launch afterwards is unaffected.

import Foundation

enum LaunchScreen: String {
    case settings
    case calendar
    /// The first bot in the list, which is the most recently active one.
    case chat
    /// The first project in the list, which the server sorts loudest-first.
    case project
    /// The first project that has a parent. Its owner and default lead are
    /// inherited rather than its own, which is the reading ("via <ancestor>")
    /// a root's screen can never show.
    case childProject = "project-child"
    /// That project, with its Add Fact sheet already up.
    case addFact = "add-fact"
    /// The same sheet opened on `recurring`, the kind whose extra inputs prove
    /// the sheet is driven by FactKind.fields rather than by a fixed form.
    case addFactRecurring = "add-fact-recurring"

    static let key = "openScreen"

    var opensAddFact: Bool { self == .addFact || self == .addFactRecurring }

    var factKind: FactKind { self == .addFactRecurring ? .recurring : .deadline }

    static var requested: LaunchScreen? {
        UserDefaults.standard.string(forKey: key).flatMap(LaunchScreen.init(rawValue:))
    }
}
