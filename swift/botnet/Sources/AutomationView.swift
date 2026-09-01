// AutomationView.swift — one automation's pane: what it is, when it runs,
// whether it is healthy, and its recent runs with their envelopes. The stdd
// automations service owns all of it; this pane is the consumer surface for
// the runs API the bridge exposes through botnetd.

import AppKit
import SwiftUI

struct AutomationView: View {
    @EnvironmentObject var store: AppStore
    let automation: Automation
    /// Snapshot-only: open with this run's detail already disclosed — a state
    /// a click can't produce offscreen. Nil in the app.
    var initialDisclosedRunID: String? = nil

    @State private var disclosedRunID: String?
    // The transient notice (a 409, a Cursor fallback) — deliberately not
    // store.lastError: neither is an error state, per the contract.
    @State private var notice: String?
    @State private var noticeClear: Task<Void, Never>?

    init(automation: Automation, initialDisclosedRunID: String? = nil) {
        self.automation = automation
        self.initialDisclosedRunID = initialDisclosedRunID
        _disclosedRunID = State(initialValue: initialDisclosedRunID)
    }

    /// Any run of this automation still settling: a manual run this client
    /// started, or an unfinished run the detail fetch reported (a scheduled
    /// fire, or a manual run from another client). Both disable Run now.
    private var inFlight: Bool {
        store.manualRunsInFlight.contains(automation.name)
            || (automation.runs ?? []).contains { !$0.isFinished }
            || automation.lastRun.map { !$0.isFinished } == true
    }

    var body: some View {
        VStack(spacing: 0) {
            header
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    about
                    if let notice {
                        Label(notice, systemImage: "info.circle")
                            .font(TypeScale.rowMeta)
                            .foregroundStyle(Palette.secondaryText)
                    }
                    runsSection
                }
                .frame(maxWidth: Metric.automationListWidth, alignment: .leading)
                .padding(.horizontal, Metric.transcriptHPad)
                .padding(.vertical, Metric.transcriptVPad)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .background(Palette.chrome)
        // The registry and the runs are shared state another client (or the
        // schedule itself) can change between visits; re-read on every open
        // and on every switch to a different automation.
        .task(id: automation.name) { await store.loadAutomationDetail(automation.name) }
        .onChange(of: automation.name) {
            disclosedRunID = nil
            notice = nil
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            Text(automation.name)
                .font(TypeScale.headerTitle)
                .foregroundStyle(Palette.primaryText)
            freshnessBadge
            Spacer()
            if let path = automation.path {
                Button {
                    Task {
                        if !FolderOpener.openInCursor(path) {
                            flash("Cursor didn't open — revealed the folder in Finder instead.")
                        }
                    }
                } label: {
                    Image(systemName: "cursorarrow.square")
                        .foregroundStyle(Palette.secondaryText)
                }
                .buttonStyle(.borderless)
                .help("Open \(path) in Cursor")
            }
            runNowButton
        }
        .padding(.horizontal, Metric.transcriptHPad)
        .frame(height: Metric.headerHeight)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
    }

    // The same dot the sidebar row wears, with its word next to it — the pane
    // has room to say "stale" where the row could only color it.
    private var freshnessBadge: some View {
        HStack(spacing: 5) {
            Circle()
                .fill(Palette.freshness(automation.freshness))
                .frame(width: Metric.freshnessDot, height: Metric.freshnessDot)
            Text(automation.freshness)
                .font(TypeScale.rowMeta)
                .foregroundStyle(Palette.secondaryText)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 3)
        .background(Palette.fieldFill, in: Capsule())
    }

    private var runNowButton: some View {
        Button {
            Task {
                if let text = await store.runNow(automation.name) { flash(text) }
            }
        } label: {
            HStack(spacing: 5) {
                if inFlight {
                    ProgressView().controlSize(.small)
                }
                Text(inFlight ? "Running…" : "Run now")
            }
        }
        .disabled(inFlight)
        .help(inFlight ? "A run is in flight" : "Start a manual run")
    }

    @ViewBuilder
    private var about: some View {
        VStack(alignment: .leading, spacing: 6) {
            if !automation.goal.isEmpty {
                Text(automation.goal)
                    .font(TypeScale.message)
                    .foregroundStyle(Palette.primaryText)
            }
            if let schedule = automation.schedule {
                Label(schedule.summary, systemImage: "clock.arrow.circlepath")
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.secondaryText)
            }
            // The manifest HAS a schedule block but it didn't parse; the
            // automation runs manually only until the manifest is fixed, and
            // the error names the field — verbatim, it is the fix.
            if let error = automation.scheduleError {
                Label(error, systemImage: "exclamationmark.triangle")
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.attention)
            }
        }
    }

    @ViewBuilder
    private var runsSection: some View {
        Text("Runs")
            .font(TypeScale.sectionLabel)
            .foregroundStyle(Palette.secondaryText)
        if let runs = automation.runs, !runs.isEmpty {
            VStack(alignment: .leading, spacing: Metric.eventRowGap) {
                ForEach(runs) { run in
                    RunRow(run: run, disclosed: disclosedRunID == run.id) {
                        toggle(run)
                    }
                    if disclosedRunID == run.id {
                        RunDetailBox(detail: store.runDetails[run.id])
                    }
                }
            }
        } else {
            Text(automation.runs == nil ? "Loading runs…" : "No runs yet.")
                .font(TypeScale.rowPreview)
                .foregroundStyle(Palette.secondaryText)
        }
    }

    private func toggle(_ run: RunSummary) {
        guard disclosedRunID != run.id else {
            disclosedRunID = nil
            return
        }
        disclosedRunID = run.id
        Task { await store.loadRunDetail(run.id) }
    }

    private func flash(_ text: String) {
        notice = text
        noticeClear?.cancel()
        noticeClear = Task {
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            if !Task.isCancelled { notice = nil }
        }
    }
}

// One run's summary line: when, how long, what asked for it, how it ended.
private struct RunRow: View {
    let run: RunSummary
    let disclosed: Bool
    let toggle: () -> Void

    @State private var hovering = false

    var body: some View {
        Button(action: toggle) {
            HStack(spacing: 10) {
                Image(systemName: "chevron.right")
                    .font(TypeScale.sectionChevron)
                    .rotationEffect(.degrees(disclosed ? 90 : 0))
                    .foregroundStyle(Palette.secondaryText)
                    .frame(width: Metric.sectionChevronWidth)
                Text(Self.startedText(run))
                    .font(TypeScale.rowPreview)
                    .foregroundStyle(Palette.primaryText)
                if let duration = run.duration {
                    Text(Self.durationText(duration))
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                Text(run.trigger)
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.secondaryText)
                if run.formUsed > 0 {
                    Text("form \(run.formUsed)")
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                Spacer(minLength: 0)
                if run.isFinished {
                    Text("exit \(run.exitCode)")
                        .font(TypeScale.messageMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
                Text(run.status)
                    .font(TypeScale.rowMeta.weight(.semibold))
                    .foregroundStyle(Palette.runStatus(run.status))
            }
            .padding(.vertical, Metric.rowVPad)
            .padding(.horizontal, Metric.sidebarGutter)
            .contentShape(RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous))
            .background(
                hovering ? Palette.rowHover : .clear,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
    }

    private static func startedText(_ run: RunSummary) -> String {
        guard let started = run.startedAt else { return run.started }
        return started.formatted(.relative(presentation: .named))
    }

    private static func durationText(_ duration: TimeInterval) -> String {
        let s = Int(duration.rounded())
        guard s >= 60 else { return "\(s)s" }
        return String(format: "%dm %02ds", s / 60, s % 60)
    }
}

// The disclosed run: envelope artifacts, escalation, error, stderr tail. Nil
// detail means the fetch is still in flight (or failed — the shared alert
// already said so); the box says which state it is in rather than vanishing.
private struct RunDetailBox: View {
    let detail: RunDetail?

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let detail {
                if let envelope = detail.envelope, !envelope.artifacts.isEmpty {
                    artifactsTable(envelope.artifacts)
                }
                if let reason = detail.summary.escalationReason {
                    Label(reason, systemImage: "exclamationmark.triangle")
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.attention)
                }
                if !detail.error.isEmpty {
                    Text(detail.error)
                        .font(TypeScale.rowPreview)
                        .foregroundStyle(Palette.calendarRed)
                        .textSelection(.enabled)
                }
                if !detail.stderrTail.isEmpty {
                    stderrBox(detail.stderrTail)
                }
                if detail.envelope == nil && detail.summary.escalationReason == nil
                    && detail.error.isEmpty && detail.stderrTail.isEmpty {
                    Text("This run reported nothing beyond its exit.")
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.secondaryText)
                }
            } else {
                Text("Loading run…")
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.secondaryText)
            }
        }
        .padding(Metric.inspectorPad)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            Palette.fieldFill,
            in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
        )
        .padding(.leading, Metric.sidebarIndent + Metric.sectionChevronWidth)
    }

    // What the run says it produced: one row per artifact. Verbatim paths in
    // monospace — a path is an address, not prose.
    private func artifactsTable(_ artifacts: [RunArtifact]) -> some View {
        Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 3) {
            GridRow {
                Text("Artifact").gridColumnAlignment(.leading)
                Text("Rows").gridColumnAlignment(.trailing)
                Text("Newest").gridColumnAlignment(.leading)
            }
            .font(TypeScale.rowMeta)
            .foregroundStyle(Palette.secondaryText)
            ForEach(artifacts, id: \.self) { artifact in
                GridRow {
                    Text(artifact.path).font(TypeScale.codeBlock)
                    Text("\(artifact.rows)").font(TypeScale.codeBlock)
                    Text(artifact.newest).font(TypeScale.codeBlock)
                }
                .foregroundStyle(Palette.primaryText)
            }
        }
    }

    // The last 8KB the driver wrote to stderr, scrolling inside its own box
    // so a noisy run can't stretch the page.
    private func stderrBox(_ tail: String) -> some View {
        ScrollView {
            Text(tail)
                .font(TypeScale.codeBlock)
                .foregroundStyle(Palette.primaryText)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(8)
        }
        .frame(maxHeight: Metric.stderrMaxHeight)
        .background(
            Palette.chrome,
            in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
        )
    }
}

/// Opens an automation's folder in Cursor — `open -a Cursor <path>`, which
/// opens a new Cursor window per folder — falling back to revealing it in
/// Finder when Cursor is missing (`open` exits nonzero). Both effects are
/// closures so a scratch harness can prove the wiring without launching
/// anything; nothing in the snapshot path ever calls this.
enum FolderOpener {
    /// Runs a tool to completion and returns its exit status; -1 when it
    /// could not start at all.
    static var launch: (_ tool: String, _ arguments: [String]) -> Int32 = { tool, arguments in
        let process = Process()
        process.executableURL = URL(fileURLWithPath: tool)
        process.arguments = arguments
        do {
            try process.run()
        } catch {
            return -1
        }
        process.waitUntilExit()
        return process.terminationStatus
    }

    static var reveal: (_ path: String) -> Void = { path in
        NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
    }

    /// True when Cursor opened; false when it fell back to Finder — the
    /// caller decides whether that deserves a notice.
    @discardableResult
    static func openInCursor(_ path: String) -> Bool {
        guard launch("/usr/bin/open", ["-a", "Cursor", path]) == 0 else {
            reveal(path)
            return false
        }
        return true
    }
}
