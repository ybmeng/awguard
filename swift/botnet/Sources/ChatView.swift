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
    }

    private var header: some View {
        HStack(spacing: 8) {
            BotAvatar(botID: bot.id, size: Metric.avatarSmall)
            Text(bot.displayName)
                .font(TypeScale.headerTitle)
                .foregroundStyle(Palette.primaryText)
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
                .onChange(of: messages.count) {
                    if let last = turns.last?.lastBubbleID {
                        withAnimation { proxy.scrollTo(last, anchor: .bottom) }
                    }
                }
            }
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
                Text(bubble.text)
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

    private var memory: String { bot.memory ?? "" }

    var body: some View {
        VStack(spacing: 0) {
            header
            // Collapsing only hides the body; `editing` and `draft` stay put,
            // so expanding again lands back in the untouched editor.
            if expanded {
                if editing { editor } else { reader }
            }
            Spacer(minLength: 0)
        }
        .background(Palette.chrome)
        .navigationTitle("Details")
        // Switching bots must never carry one bot's unsaved draft to another.
        .onChange(of: bot.id) { editing = false }
    }

    private var header: some View {
        HStack {
            Button {
                withAnimation(.easeOut(duration: 0.12)) { expanded.toggle() }
            } label: {
                HStack(spacing: 5) {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 9, weight: .semibold))
                        .foregroundStyle(Palette.secondaryText)
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                    Text("Memory")
                        .font(TypeScale.headerTitle)
                        .foregroundStyle(Palette.primaryText)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(expanded ? "Collapse memory" : "Expand memory")
            Spacer()
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
        .padding(.horizontal, Metric.inspectorPad)
        .frame(height: Metric.headerHeight)
        .overlay(alignment: .bottom) {
            Rectangle().fill(Palette.hairline).frame(height: 1)
        }
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
