// lead-check.swift — asserts what the Add Fact sheet's lead row says, against
// Projects decoded by the real APIClient decoder from real wire shapes.
// Compiled against the real Sources/ files by dev/lead-check.sh, the same
// stance as the Mac's decode-check.
//
// This exists because the interesting value cannot be screenshotted: reaching a
// draft of 0 means tapping a stepper 180 times, and simctl has no tap. The rule
// it guards is a server rule (createFact in go/botnet/projects.go substitutes
// the project's EffectiveLeadDays whenever a dated fact arrives with
// leadDays == 0), so a row reading "Lead 0 days" would promise a window the
// server will not keep.

import Foundation

@main
struct LeadCheck {
    static var failures = 0

    static let stamps = #""createdAt":"2026-09-02T00:00:00Z","updatedAt":"2026-09-02T00:00:00Z""#

    static func project(_ json: String) -> Project {
        try! APIClient().decoder.decode(Project.self, from: json.data(using: .utf8)!)
    }

    static func expect(_ actual: String, _ expected: String, _ what: String) {
        if actual == expected {
            print("PASS \(what): \(actual)")
        } else {
            print("FAIL \(what): expected \(expected), got \(actual)")
            failures += 1
        }
    }

    static func expect(_ actual: Int, _ expected: Int, _ what: String) {
        expect(String(actual), String(expected), what)
    }

    static func main() {
        // A project whose ancestor sets 180: the server rolls that up into
        // effectiveLeadDays, and the sheet must open there rather than on a
        // number of its own.
        let inheriting = project("""
        {"id":"prj_1","name":"Office lease","defaultLeadDays":0,"effectiveLeadDays":180,\(stamps)}
        """)
        // A project that sets its own.
        let ownLead = project("""
        {"id":"prj_2","name":"Shanghai WFOE","defaultLeadDays":180,"effectiveLeadDays":180,\(stamps)}
        """)
        // A botnetd predating thresholds sends neither key, so nothing is
        // derived and the global fallback is the only honest number.
        let oldServer = project("""
        {"id":"prj_3","name":"Legacy",\(stamps)}
        """)

        expect(FactLead.initialDraft(for: inheriting), 180, "inheriting project opens at its rolled-up lead")
        expect(FactLead.initialDraft(for: ownLead), 180, "own-lead project opens at its own")
        expect(FactLead.initialDraft(for: oldServer), 30, "old server falls back to the global default")

        // The row a user reaches by stepping to 0. It must name the number the
        // server will actually store, never "0 days".
        expect(FactLead.label(draft: 0, project: inheriting), "Lead: inherited (180 d)", "draft 0, inheriting")
        expect(FactLead.label(draft: 0, project: oldServer), "Lead: inherited (30 d)", "draft 0, old server")

        expect(FactLead.label(draft: 1, project: inheriting), "Lead 1 day", "singular")
        expect(FactLead.label(draft: 14, project: inheriting), "Lead 14 days", "plural")
        expect(FactLead.label(draft: 180, project: inheriting), "Lead 180 days", "the seeded value reads as itself")

        if failures > 0 {
            print("lead-check: \(failures) failed")
            exit(1)
        }
        print("lead-check: all passed")
    }
}
