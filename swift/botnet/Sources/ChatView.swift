// ChatView.swift — the bot chat panel: header, bubble transcript, composer. A
// pure pane: the details inspector is window chrome and belongs to whoever owns
// the column. State is fetched from and written to botnetd; this view holds only
// the draft text. The header's actions menu carries the per-bot operations
// (compact, and broken-model recovery).

import SwiftUI

struct ChatView: View {
    @EnvironmentObject var store: AppStore
    let bot: Bot
    @Binding var showDetails: Bool

    @State private var draft = ""
    @State private var renaming = false
    @State private var renameDraft = ""
    @FocusState private var renameFocused: Bool

    private var messages: [Message] { store.messages(for: bot.id) }
    private var turns: [ChatTurn] { ChatTurn.build(from: messages) }
    private var pending: Bool { store.pendingBotIDs.contains(bot.id) }
    private var compacting: Bool { store.compactingBotIDs.contains(bot.id) }
    private var canSend: Bool {
        !pending && !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        VStack(spacing: 0) {
            header
            transcript
            composer
        }
        .background(Palette.chrome)
        .task(id: bot.id) { await store.loadConversation(bot.id) }
        // An in-progress rename must never carry one bot's draft to another.
        .onChange(of: bot.id) {
            renaming = false
            renameDraft = ""
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            BotAvatar(botID: bot.id, size: Metric.avatarSmall)
            if renaming {
                TextField("", text: $renameDraft)
                    .font(TypeScale.headerTitle)
                    .foregroundStyle(Palette.primaryText)
                    .textFieldStyle(.plain)
                    .focused($renameFocused)
                    .onSubmit(commitRename)
                    .onExitCommand { renaming = false }
                    .onAppear { renameFocused = true }
            } else {
                Text(bot.displayName)
                    .font(TypeScale.headerTitle)
                    .foregroundStyle(Palette.primaryText)
                Button {
                    renameDraft = bot.displayName
                    renaming = true
                } label: {
                    Image(systemName: "pencil")
                        .foregroundStyle(Palette.secondaryText)
                }
                .buttonStyle(.borderless)
                .help("Rename bot")
                .accessibilityIdentifier("renameBotButton")
            }
            Spacer()
            actionsMenu
            Button { showDetails.toggle() } label: {
                Image(systemName: "sidebar.right")
                    .foregroundStyle(Palette.secondaryText)
            }
            .buttonStyle(.borderless)
            .help("Bot details")
        }
        .padding(.horizontal, Metric.transcriptHPad)
        .frame(height: Metric.headerHeight)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
    }

    // Per-bot operations that used to be rows in the details inspector. Compact
    // seals the conversation's memory; the model submenu appears only when the
    // bot's model has gone away, the one state with no other route to a fix.
    private var actionsMenu: some View {
        Menu {
            Button {
                Task { await store.compact(bot) }
            } label: {
                Label(
                    compacting ? "Compacting…" : "Compact conversation",
                    systemImage: "arrow.down.right.and.arrow.up.left"
                )
            }
            .disabled(compacting || messages.isEmpty)

            if bot.isModelBroken {
                Menu("Change model") {
                    ForEach(store.models) { option in
                        Button(option.name) {
                            Task { await store.updateBot(bot, fields: ["model": option.id]) }
                        }
                    }
                }
            }
        } label: {
            Image(systemName: "ellipsis")
                .foregroundStyle(Palette.secondaryText)
        }
        .menuStyle(.borderlessButton)
        .menuIndicator(.hidden)
        .fixedSize()
        .help("Bot actions")
    }

    private var transcript: some View {
        GeometryReader { geo in
            let content = max(0, geo.size.width - 2 * Metric.transcriptHPad)
            let gutter = content * (1 - Metric.bubbleWidthFraction)
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(turns.enumerated()), id: \.element.id) { index, turn in
                            TurnView(turn: turn, gutter: gutter, botID: bot.id)
                                .padding(.top, index == 0 ? 0 : Metric.turnGap)
                        }
                        if pending {
                            ThinkingBubble()
                                .padding(.top, turns.isEmpty ? 0 : Metric.turnGap)
                                .id("pending")
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, Metric.transcriptHPad)
                    .padding(.vertical, Metric.transcriptVPad)
                }
                // Opens at the newest turn and stays pinned while the reader is
                // at the bottom; scrolling up unpins, as a chat should.
                .defaultScrollAnchor(.bottom)
                // The explicit scroll covers the reader who scrolled up and then
                // sent: the pin is off, but their own send must snap back down.
                // `pending` is a trigger too — the thinking bubble appears
                // without changing the message count.
                .onChange(of: messages.count) { scrollToNewest(proxy) }
                .onChange(of: pending) { scrollToNewest(proxy) }
            }
        }
    }

    // Deferred one update: onChange fires before the LazyVStack lays out the
    // row it was told about, and scrollTo to an id with no laid-out row is a
    // no-op.
    private func scrollToNewest(_ proxy: ScrollViewProxy) {
        guard let target = pending ? "pending" : turns.last?.lastBubbleID else { return }
        Task { @MainActor in
            withAnimation { proxy.scrollTo(target, anchor: .bottom) }
        }
    }

    private var composer: some View {
        HStack(spacing: 8) {
            Button {} label: {
                Image(systemName: "plus")
                    .foregroundStyle(Palette.secondaryText)
                    .frame(width: circleDiameter, height: circleDiameter)
                    .background(Palette.fieldFill, in: Circle())
            }
            .buttonStyle(.plain)
            .help("Attach")

            TextField("Message \(bot.displayName)…", text: $draft, axis: .vertical)
                .textFieldStyle(.plain)
                .font(TypeScale.composer)
                .foregroundStyle(Palette.primaryText)
                .lineLimit(1...6)
                .onSubmit(sendDraft)

            Button(action: sendDraft) {
                Image(systemName: "arrow.up")
                    .foregroundStyle(canSend ? Palette.userBubbleText : Palette.secondaryText)
                    .frame(width: circleDiameter, height: circleDiameter)
                    .background(canSend ? Palette.userBubble : Palette.fieldFill, in: Circle())
            }
            .buttonStyle(.plain)
            .disabled(!canSend)
        }
        .padding(.horizontal, 8)
        .frame(minHeight: Metric.composerMinHeight)
        .background {
            RoundedRectangle(cornerRadius: Metric.composerRadius, style: .continuous)
                .fill(Palette.chrome)
                .overlay {
                    RoundedRectangle(cornerRadius: Metric.composerRadius, style: .continuous)
                        .strokeBorder(Palette.fieldStroke, lineWidth: 1)
                }
        }
        .padding(Metric.composerPad)
    }

    private var circleDiameter: CGFloat { Metric.composerMinHeight - 20 }

    // A failed save keeps the field open so the name isn't lost; the error
    // itself surfaces through the store's alert. An empty or unchanged name
    // just ends the edit — no patch to send.
    private func commitRename() {
        let trimmed = renameDraft.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty, trimmed != bot.displayName else {
            renaming = false
            return
        }
        Task {
            if await store.updateBot(bot, fields: ["displayName": trimmed]) {
                renaming = false
            }
        }
    }

    private func sendDraft() {
        let text = draft
        draft = ""
        Task { await store.send(text, to: bot) }
    }
}

private struct TurnView: View {
    let turn: ChatTurn
    /// Empty space reserved on the far side of the bubbles. Capping the bubble
    /// itself with a `maxWidth` frame would also stretch short bubbles out to
    /// that width; reserving the gutter caps the long ones and leaves the short
    /// ones sized to their text.
    let gutter: CGFloat
    let botID: String

    private var isUser: Bool { turn.role == .user }

    var body: some View {
        HStack(spacing: 0) {
            if isUser { Spacer(minLength: gutter) }
            VStack(alignment: isUser ? .trailing : .leading, spacing: Metric.bubbleGap) {
                ForEach(turn.messages) { message in
                    MessageView(message: message, isUser: isUser, botID: botID)
                }
            }
            .layoutPriority(1)
            if !isUser { Spacer(minLength: gutter) }
        }
    }
}

// One message: its paragraph bubbles, then a details row carrying the message
// id. The row is where per-message metadata accumulates, so it is a disclosure
// rather than a label — collapsed it is a single quiet line.
private struct MessageView: View {
    let message: TranscriptMessage
    let isUser: Bool
    let botID: String

    @EnvironmentObject private var store: AppStore
    @State private var expanded = false
    @State private var hovering = false

    var body: some View {
        VStack(alignment: isUser ? .trailing : .leading, spacing: 2) {
            ForEach(message.bubbles) { bubble in
                bubbleText(bubble.text)
                    .font(TypeScale.message)
                    .foregroundStyle(isUser ? Palette.userBubbleText : Palette.botBubbleText)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, Metric.bubbleHPad)
                    .padding(.vertical, Metric.bubbleVPad)
                    .background(
                        isUser ? Palette.userBubble : Palette.botBubble,
                        in: RoundedRectangle(cornerRadius: Metric.bubbleRadius, style: .continuous)
                    )
                    .opacity(message.isAwaiting ? 0.55 : 1)
                    .id(bubble.id)
            }

            // The audit trail sits beneath the answer, never on a user turn.
            // When the reply's tool calls are recorded, the tool-call list shows
            // each one — including a search's own sources — so the aggregate
            // Sources row is dropped to avoid listing the same links twice. A
            // reply from a server that only reports citations (the
            // openrouter:web_search fallback, no per-call record) still shows
            // Sources as before. The common reply carries neither and nothing draws.
            if !isUser {
                if !message.toolCalls.isEmpty {
                    ToolCallsView(toolCalls: message.toolCalls)
                } else if !message.citations.isEmpty {
                    SourcesView(citations: message.citations)
                }
            }

            if let failure = message.failureText {
                HStack(spacing: 5) {
                    Image(systemName: "exclamationmark.circle")
                    Text(failure)
                    Button("Retry") { Task { await store.retry(message.id, on: botID) } }
                        .buttonStyle(.plain)
                        .underline()
                }
                .font(TypeScale.rowMeta)
                .foregroundStyle(Palette.attention)
                .padding(.horizontal, 2)
            }

            details
        }
    }

    // The model routinely writes inline markdown (**bold**, *italic*, `code`,
    // links). SwiftUI only auto-parses markdown from a LocalizedStringKey
    // literal, never a String, so parse it ourselves. User bubbles stay
    // literal: a person typing "2 * 3" or "*sigh*" means the characters, not
    // emphasis, and their text is shown exactly as sent.
    private func bubbleText(_ text: String) -> Text {
        isUser ? Text(text) : Text(Self.render(markdown: text))
    }

    // inlineOnlyPreservingWhitespace keeps the single newlines a bubble may
    // hold (paragraphs already split on blank lines upstream) and leaves block
    // syntax as literal text rather than laying out lists or code fences a
    // bubble can't frame. On a parse failure the raw text is shown unchanged.
    private static func render(markdown text: String) -> AttributedString {
        (try? AttributedString(
            markdown: text,
            options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        )) ?? AttributedString(text)
    }

    private var details: some View {
        VStack(alignment: isUser ? .trailing : .leading, spacing: 2) {
            Button {
                expanded.toggle()
            } label: {
                HStack(spacing: 3) {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 8, weight: .semibold))
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                    Text(message.isAwaiting ? "sending…" : message.id)
                        .font(TypeScale.messageMeta)
                        .lineLimit(1)
                }
                .foregroundStyle(Palette.secondaryText)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            // Always present, since the id is the handle for everything that
            // will accumulate in this row. Held back at rest so a column of ids
            // never competes with the conversation.
            .opacity(hovering || expanded || message.didFail ? 1 : 0.45)

            if expanded {
                VStack(alignment: isUser ? .trailing : .leading, spacing: 1) {
                    detail("id", message.id)
                    if let status = message.status?.rawValue { detail("status", status) }
                }
                .padding(.top, 1)
            }
        }
        .padding(.horizontal, 4)
        .animation(.easeOut(duration: 0.12), value: expanded)
        .onHover { hovering = $0 }
    }

    private func detail(_ label: String, _ value: String) -> some View {
        HStack(spacing: 4) {
            Text(label)
                .font(TypeScale.messageMeta)
                .foregroundStyle(Palette.secondaryText.opacity(0.7))
            Text(value)
                .font(TypeScale.messageMeta)
                .foregroundStyle(Palette.secondaryText)
                .textSelection(.enabled)
        }
    }
}

// The web sources behind a bot reply, as a "Sources (n)" disclosure: the count
// row toggles a numbered list of links, each opening its url in the browser.
// Shown only when a bot message carries citations, so it stays out of the way on
// the common turn. A search turn can return ~20 sources, so a long list starts
// collapsed and never buries the reply — and nothing here nests a scroll inside
// the transcript, which is itself a ScrollView.
private struct SourcesView: View {
    let citations: [Citation]
    @Environment(\.openURL) private var openURL
    @State private var expanded: Bool

    init(citations: [Citation]) {
        self.citations = citations
        // A handful shows inline; a search turn's ~20 sources stay folded so the
        // list never buries the reply. The count is the handle to open them.
        _expanded = State(initialValue: citations.count <= 3)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Button {
                expanded.toggle()
            } label: {
                HStack(spacing: 3) {
                    Image(systemName: "chevron.right")
                        .font(TypeScale.sectionChevron)
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                    Text("Sources (\(citations.count))")
                        .font(TypeScale.rowMeta)
                }
                .foregroundStyle(Palette.secondaryText)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(expanded ? "Hide sources" : "Show sources")

            if expanded {
                ForEach(Array(citations.enumerated()), id: \.offset) { index, citation in
                    SourceLink(number: index + 1, citation: citation)
                }
            }
        }
        .padding(.horizontal, 2)
        .padding(.top, 2)
        .animation(.easeOut(duration: 0.12), value: expanded)
    }
}

// One numbered source row that opens its url in the browser. Shared by the
// aggregate Sources disclosure and a web_search tool call's expanded results, so
// both read identically. Title carries the weight; the host trails it whole so
// the domain — what a reader judges a source by — survives a long title's
// truncation, and is skipped when the title already fell back to the host.
private struct SourceLink: View {
    let number: Int
    let citation: Citation
    @Environment(\.openURL) private var openURL

    var body: some View {
        Button {
            if let url = citation.link { openURL(url) }
        } label: {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text("\(number)")
                    .font(TypeScale.messageMeta)
                    .foregroundStyle(Palette.secondaryText)
                Text(citation.displayTitle)
                    .font(TypeScale.rowMeta)
                    .foregroundStyle(Palette.botBubbleText)
                    .lineLimit(1)
                    .truncationMode(.tail)
                if let host = citation.host, host != citation.displayTitle {
                    Text(host)
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.secondaryText)
                        .lineLimit(1)
                        .layoutPriority(1)
                }
                Image(systemName: "arrow.up.right")
                    .font(TypeScale.sectionChevron)
                    .foregroundStyle(Palette.secondaryText)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .help(citation.url)
    }
}

// The tool calls behind a bot reply, as a "Tool calls (n)" disclosure mirroring
// the Sources row. Each entry is a ToolCallRow — an icon, the tool, and a
// one-line summary — that expands to its own detail. A turn rarely runs more
// than a couple of tools, but the list stays collapsible so a busy turn never
// buries the reply, and nothing here nests a scroll inside the transcript, which
// is itself a ScrollView.
private struct ToolCallsView: View {
    let toolCalls: [ToolCall]
    @State private var expanded: Bool

    init(toolCalls: [ToolCall]) {
        self.toolCalls = toolCalls
        // A handful shows inline; a longer run folds so the count is the handle.
        _expanded = State(initialValue: toolCalls.count <= 3)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Button {
                expanded.toggle()
            } label: {
                HStack(spacing: 3) {
                    Image(systemName: "chevron.right")
                        .font(TypeScale.sectionChevron)
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                    Text("Tool calls (\(toolCalls.count))")
                        .font(TypeScale.rowMeta)
                }
                .foregroundStyle(Palette.secondaryText)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(expanded ? "Hide tool calls" : "Show tool calls")

            if expanded {
                ForEach(Array(toolCalls.enumerated()), id: \.offset) { _, call in
                    ToolCallRow(call: call)
                }
            }
        }
        .padding(.horizontal, 2)
        .padding(.top, 2)
        .animation(.easeOut(duration: 0.12), value: expanded)
    }
}

// One recorded tool call: a summary line — icon, tool name, and what it did (a
// web_search's query and result count, or a memory command) — that expands to
// the call's detail. web_search reveals its sources in the same SourceLink
// styling as the Sources row; every other tool reveals the verbatim result it
// fed back to the model. Collapsed by default so the list reads at a glance.
private struct ToolCallRow: View {
    let call: ToolCall
    @State private var expanded = false

    private var isWebSearch: Bool { call.name == "web_search" }
    private var requestId: String? {
        guard isWebSearch, let rid = call.requestId, !rid.isEmpty else { return nil }
        return rid
    }
    private var hasDetail: Bool {
        !call.citations.isEmpty || !call.result.isEmpty || requestId != nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Button {
                if hasDetail { expanded.toggle() }
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 6) {
                    Image(systemName: symbol)
                        .font(TypeScale.rowMeta)
                        .foregroundStyle(Palette.secondaryText)
                        .frame(width: Metric.toolIconWidth)
                    summary
                        .font(TypeScale.rowMeta)
                        .lineLimit(1)
                        .truncationMode(.tail)
                    Spacer(minLength: 6)
                    if hasDetail {
                        Image(systemName: "chevron.right")
                            .font(TypeScale.sectionChevron)
                            .foregroundStyle(Palette.secondaryText)
                            .rotationEffect(.degrees(expanded ? 90 : 0))
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(call.arguments)

            if expanded { detail }
        }
        .animation(.easeOut(duration: 0.12), value: expanded)
    }

    // The tool's glyph: a search lens, memory's document, else a generic tool.
    private var symbol: String {
        switch call.name {
        case "web_search": return "magnifyingglass"
        case "memory": return "brain"
        default: return "wrench.and.screwdriver"
        }
    }

    // Icon aside, the row names the tool quietly and lets the informative bit —
    // the query, or the command — carry the weight, the way SourceLink leads
    // with the title. Built as one Text so it truncates as a single line.
    private var summary: Text {
        switch call.name {
        case "web_search":
            let n = call.citations.count
            let query = Text(call.query ?? "search").foregroundColor(Palette.botBubbleText)
            let countText = "\(n) result\(n == 1 ? "" : "s")"
            // With a known backend the tail names it between dots (· exa · 4
            // results); older records without one keep the bare count.
            let tail: Text
            if let backend = call.backend, !backend.isEmpty {
                tail = Text(" · \(backend) · \(countText)").foregroundColor(Palette.secondaryText)
            } else {
                tail = Text("  \(countText)").foregroundColor(Palette.secondaryText)
            }
            return Text("Web search  ").foregroundColor(Palette.secondaryText) + query + tail
        case "memory":
            let command = Text(call.command ?? "—").foregroundColor(Palette.botBubbleText)
            return Text("Memory  ").foregroundColor(Palette.secondaryText) + command
        default:
            return Text(call.name).foregroundColor(Palette.botBubbleText)
        }
    }

    @ViewBuilder private var detail: some View {
        VStack(alignment: .leading, spacing: 4) {
            if isWebSearch, !call.citations.isEmpty {
                VStack(alignment: .leading, spacing: 3) {
                    ForEach(Array(call.citations.enumerated()), id: \.offset) { index, citation in
                        SourceLink(number: index + 1, citation: citation)
                    }
                }
            } else if !call.result.isEmpty {
                // Every non-search tool (and a search that returned no sources)
                // reveals the exact string it handed back to the model.
                MachineText(text: call.result)
            }
            // Provider request id, for debugging — subtle monospace, selectable
            // in full even when truncated; only on a search that recorded one.
            if let rid = requestId {
                let prefix = (call.backend.map { $0.isEmpty ? "" : "\($0) · " } ?? "")
                Text("\(prefix)\(rid)")
                    .font(TypeScale.codeBlock)
                    .foregroundStyle(Palette.secondaryText)
                    .textSelection(.enabled)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .help("Provider request id")
            }
        }
        .padding(.top, 1)
    }
}

private struct ThinkingBubble: View {
    var body: some View {
        HStack(spacing: 0) {
            HStack(spacing: 6) {
                ProgressView().controlSize(.small)
                Text("thinking…")
                    .font(TypeScale.message)
                    .foregroundStyle(Palette.secondaryText)
            }
            .padding(.horizontal, Metric.bubbleHPad)
            .padding(.vertical, Metric.bubbleVPad)
            .background(
                Palette.botBubble,
                in: RoundedRectangle(cornerRadius: Metric.bubbleRadius, style: .continuous)
            )
            Spacer(minLength: 0)
        }
    }
}

// One collapsible inspector section, drawn as a rounded card: a header that is
// the toggle (rotating chevron + title, with an optional trailing accessory
// that stays clickable on its own), and a body shown only while expanded, with
// one hairline between the two. The card treatment — radius, border, clip —
// lives entirely here so restyling every section is a one-place edit; neighbors
// are separated by the caller's stack spacing, not by hairlines. Collapsing
// removes the content view — state that must survive a collapse/expand
// round-trip (like Memory's draft) belongs to the caller, not inside the
// content closure.
struct InspectorSection<Content: View, Accessory: View>: View {
    let title: String
    @Binding var expanded: Bool
    @ViewBuilder let content: () -> Content
    @ViewBuilder let accessory: () -> Accessory

    var body: some View {
        VStack(spacing: 0) {
            header
            if expanded {
                hairline
                content()
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: Metric.inspectorCardRadius, style: .continuous))
        .overlay {
            RoundedRectangle(cornerRadius: Metric.inspectorCardRadius, style: .continuous)
                .strokeBorder(Palette.fieldStroke, lineWidth: 1)
        }
    }

    private var header: some View {
        HStack {
            Button {
                withAnimation(.easeOut(duration: 0.12)) { expanded.toggle() }
            } label: {
                HStack(spacing: 5) {
                    Image(systemName: "chevron.right")
                        .font(TypeScale.sectionChevron)
                        .foregroundStyle(Palette.secondaryText)
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                    Text(title)
                        .font(TypeScale.headerTitle)
                        .foregroundStyle(Palette.primaryText)
                    Spacer()
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(expanded ? "Collapse \(title.lowercased())" : "Expand \(title.lowercased())")
            accessory()
        }
        .padding(.horizontal, Metric.inspectorPad)
        .frame(height: Metric.headerHeight)
    }

    private var hairline: some View {
        Rectangle().fill(Palette.hairline).frame(height: 1)
    }
}

extension InspectorSection where Accessory == EmptyView {
    init(title: String, expanded: Binding<Bool>, @ViewBuilder content: @escaping () -> Content) {
        self.init(title: title, expanded: expanded, content: content, accessory: { EmptyView() })
    }
}

// The details inspector: the bot's memory, readable at rest and editable behind
// the pencil. Saves are explicit — the server's model calls can also write
// memory, and last-write-wins is the accepted semantics, so nothing autosaves.
struct BotDetails: View {
    @EnvironmentObject var store: AppStore
    let bot: Bot
    @Binding var expanded: Bool

    @State private var editing = false
    @State private var draft = ""
    @State private var saving = false
    @State private var toolsExpanded = true

    private var memory: String { bot.memory ?? "" }

    var body: some View {
        VStack(spacing: Metric.inspectorCardSpacing) {
            // Collapsing only hides the body; `editing` and `draft` live on
            // this view, not in the section content, so expanding again lands
            // back in the untouched editor.
            InspectorSection(title: "Memory", expanded: $expanded) {
                if editing { editor } else { reader }
            } accessory: {
                if expanded && !editing {
                    Button {
                        draft = memory
                        editing = true
                    } label: {
                        Image(systemName: "pencil")
                            .foregroundStyle(Palette.secondaryText)
                    }
                    .buttonStyle(.borderless)
                    .help("Edit memory")
                }
            }
            // Absent entirely (not empty) until the server has answered; an
            // older botnetd without the route keeps `tools` nil for the run.
            if let tools = store.tools {
                InspectorSection(title: "Tools", expanded: $toolsExpanded) {
                    toolsBody(tools)
                }
            }
            Spacer(minLength: 0)
        }
        .padding(Metric.inspectorCardInset)
        .background(Palette.chrome)
        .navigationTitle("Details")
        // Switching bots must never carry one bot's unsaved draft to another.
        .onChange(of: bot.id) { editing = false }
        .task { await store.loadTools() }
    }

    private var reader: some View {
        Group {
            if memory.isEmpty {
                ContentUnavailableView {
                    Label("No memory yet", systemImage: "pencil")
                } description: {
                    Text("Use the pencil to write some, or let the bot earn its own.")
                }
                .frame(maxWidth: .infinity, maxHeight: Metric.inspectorSectionMaxHeight)
            } else {
                ScrollView {
                    Text(memory)
                        .font(TypeScale.message)
                        .foregroundStyle(Palette.primaryText)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(Metric.inspectorPad)
                }
                .frame(maxHeight: Metric.inspectorSectionMaxHeight)
            }
        }
    }

    private var editor: some View {
        VStack(spacing: Metric.inspectorPad) {
            TextEditor(text: $draft)
                .font(TypeScale.message)
                .foregroundStyle(Palette.primaryText)
                .scrollContentBackground(.hidden)
                .padding(6)
                .background(
                    Palette.fieldFill,
                    in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
                )
                .overlay {
                    RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
                        .strokeBorder(Palette.fieldStroke, lineWidth: 1)
                }
                // TextEditor scrolls itself past the cap.
                .frame(maxHeight: Metric.inspectorSectionMaxHeight)
            HStack {
                Spacer()
                Button("Cancel") { editing = false }
                    .keyboardShortcut(.cancelAction)
                    .disabled(saving)
                Button(saving ? "Saving…" : "Save", action: save)
                    .keyboardShortcut("s", modifiers: .command)
                    .disabled(saving)
            }
        }
        .padding(Metric.inspectorPad)
    }

    // What the model is told, verbatim: the description keeps its line breaks
    // and the parameters schema is shown as re-indented JSON, never paraphrased.
    // Read-only by design — these change only with the server binary.
    private func toolsBody(_ tools: [ToolDefinition]) -> some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Metric.inspectorPad) {
                ForEach(tools) { tool in
                    VStack(alignment: .leading, spacing: 6) {
                        Text(tool.displayTitle)
                            .font(TypeScale.rowTitle)
                            .foregroundStyle(Palette.primaryText)
                            .textSelection(.enabled)
                        if let fn = tool.function {
                            Text(fn.description)
                                .font(TypeScale.message)
                                .foregroundStyle(Palette.primaryText)
                                .textSelection(.enabled)
                                .fixedSize(horizontal: false, vertical: true)
                            MachineText(text: fn.parametersJSON)
                        } else {
                            // A server-side tool carries no function schema — the
                            // model runs it directly. Show its raw type in the
                            // same machine-text box so the row is honest about
                            // exactly what the model is offered.
                            MachineText(text: tool.type)
                        }
                    }
                }
                if tools.isEmpty {
                    Text("No tools")
                        .font(TypeScale.message)
                        .foregroundStyle(Palette.secondaryText)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(Metric.inspectorPad)
        }
        .frame(maxHeight: Metric.inspectorSectionMaxHeight)
    }

    private func save() {
        saving = true
        Task {
            // A failed patch keeps the editor open so the text isn't lost; the
            // error itself surfaces through the store's alert.
            if await store.updateBot(bot, fields: ["memory": draft]) {
                editing = false
            }
            saving = false
        }
    }
}

// Verbatim machine text in a boxed monospace block — a function tool's
// parameters schema, a server tool's raw type, or the result a tool call fed
// back to the model. One treatment so machine output reads as one thing
// wherever it appears.
private struct MachineText: View {
    let text: String

    var body: some View {
        Text(text)
            .font(TypeScale.codeBlock)
            .foregroundStyle(Palette.primaryText)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(6)
            .background(
                Palette.fieldFill,
                in: RoundedRectangle(cornerRadius: Metric.rowRadius, style: .continuous)
            )
    }
}
