// lead-check.swift — asserts what the Add Fact sheet's lead row says, against
// Projects decoded by the real APIClient decoder from real wire shapes.
// Compiled against the real Sources/ files by dev/lead-check.sh, the same
// stance as the Mac's decode-check.
//
// This exists because the interesting value cannot be screenshotted: reaching a
// draft of 0 means tapping a stepper 180 times, and simctl has no tap.
//
// The rule it guards is a server rule. A fact's `leadDays` of 0 means UNSET,
// and the window that actually applies is the derived `effectiveLeadDays` the
// server resolves at READ time from the owning project (go/botnet/projects.go,
// resolveFactLeads). Create stores the authored 0 verbatim rather than
// substituting — that substitution is what once made create and patch disagree
// about the same number — so 0 now means the same thing on both verbs. Hence
// two readings, both asserted below: the sheet's stepper must never print
// "Lead 0 days" for a window the fact will not get, and the pane's row must
// show the effective number and say when it is the project's.

import Foundation

@main
struct LeadCheck {
    static var failures = 0

    static let stamps = #""createdAt":"2026-09-02T00:00:00Z","updatedAt":"2026-09-02T00:00:00Z""#

    static func project(_ json: String) -> Project {
        try! APIClient().decoder.decode(Project.self, from: json.data(using: .utf8)!)
    }

    static func fact(_ json: String) -> ProjectFact {
        try! APIClient().decoder.decode(ProjectFact.self, from: json.data(using: .utf8)!)
    }

    static func expect(_ actual: String?, _ expected: String?, _ what: String) {
        if actual == expected {
            print("PASS \(what): \(actual ?? "nil")")
        } else {
            print("FAIL \(what): expected \(expected ?? "nil"), got \(actual ?? "nil")")
            failures += 1
        }
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

        expect(FactLead.initialDraft(for: inheriting), 0, "sheet opens at 0 so an untouched fact inherits")
        expect(FactLead.initialDraft(for: ownLead), 0, "own-lead project also opens at 0 (inherits its own)")
        expect(FactLead.initialDraft(for: oldServer), 0, "old server: 0 still reads as the global default")

        // The row a user reaches by stepping to 0. It must name the number the
        // server will actually store, never "0 days".
        expect(FactLead.label(draft: 0, project: inheriting), "Lead: inherited (180 d)", "draft 0, inheriting")
        expect(FactLead.label(draft: 0, project: oldServer), "Lead: inherited (30 d)", "draft 0, old server")

        expect(FactLead.label(draft: 1, project: inheriting), "Lead 1 day", "singular")
        expect(FactLead.label(draft: 14, project: inheriting), "Lead 14 days", "plural")
        expect(FactLead.label(draft: 180, project: inheriting), "Lead 180 days", "the seeded value reads as itself")

        // The FACT ROW in the pane, which is a different reading from the
        // stepper above: the row always shows the window the server actually
        // computes due_soon from (effectiveLeadDays), and marks the facts that
        // authored none of their own. "180d lead" and "180d lead (project)" are
        // the same number in two different states — only the second one follows
        // the project when its default changes — so the row has to say which.
        let ownLeadFact = fact("""
        {"id":"fct_1","projectId":"prj_1","kind":"deadline","title":"Filing","leadDays":45,"effectiveLeadDays":45,"done":false,"createdBy":"user",\(stamps)}
        """)
        let inheritingFact = fact("""
        {"id":"fct_2","projectId":"prj_1","kind":"deadline","title":"Renewal","leadDays":0,"effectiveLeadDays":180,"done":false,"createdBy":"user",\(stamps)}
        """)
        // A botnetd predating the derived key sends no effectiveLeadDays, so
        // the authored lead is the only window there is and the row must read
        // exactly as it did before the key existed — never "(project)", which
        // would claim an inheritance that server cannot perform.
        let oldServerFact = fact("""
        {"id":"fct_3","projectId":"prj_1","kind":"deadline","title":"Legacy","leadDays":45,"done":false,"createdBy":"user",\(stamps)}
        """)
        // No window derived at all. NOT the undated-kind case: a live botnetd
        // resolves an undated milestone's effectiveLeadDays to the project's
        // 180 like any other fact, and what keeps a lead off that row is the
        // pane's `if let due` gate, not this helper. This is the server that
        // derives nothing — the row then shows no lead rather than "0d lead".
        let noWindowFact = fact("""
        {"id":"fct_4","projectId":"prj_1","kind":"milestone","title":"Lease signed","leadDays":0,"effectiveLeadDays":0,"done":false,"createdBy":"user",\(stamps)}
        """)

        expect(inheritingFact.effectiveLeadDays, 180, "derived key decodes")
        expect(oldServerFact.effectiveLeadDays, 45, "absent derived key falls back to the authored lead")

        expect(FactLead.rowLabel(for: ownLeadFact), "45d lead", "row: fact sets its own")
        expect(FactLead.rowLabel(for: inheritingFact), "180d lead (project)", "row: fact inherits")
        expect(FactLead.rowLabel(for: oldServerFact), "45d lead", "row: old server reads as before")
        expect(FactLead.rowLabel(for: noWindowFact), nil, "row: server derives no window, no label")

        if failures > 0 {
            print("lead-check: \(failures) failed")
            exit(1)
        }
        print("lead-check: all passed")
    }
}
