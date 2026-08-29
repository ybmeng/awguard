// ChatView.swift — the bot chat panel: header, bubble transcript, composer. A
// pure pane: the details inspector is window chrome and belongs to whoever owns
// the column. State is fetched from and written to botnetd; this view holds only
// the draft text.

import SwiftUI

struct ChatView: View {
    @EnvironmentObject var store: AppStore
    let bot: Bot
    @Binding var showDetails: Bool

    @State private var draft = ""

    private var messages: [Message] { store.messages(for: bot.id) }
    private var turns: [ChatTurn] { ChatTurn.build(from: messages) }
    private var pending: Bool { store.pendingBotIDs.contains(bot.id) }
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

struct BotDetails: View {
    @EnvironmentObject var store: AppStore
    let bot: Bot
    let messageCount: Int

    private var chain: [Segment] { store.segmentChain(for: bot.id) }
    private var compacting: Bool { store.compactingBotIDs.contains(bot.id) }

    var body: some View {
        List {
            Section("Model") {
                Text(store.models.first(where: { $0.id == bot.model })?.name ?? bot.model)
                Text(bot.model).font(.caption).foregroundStyle(.secondary)
                if bot.isModelBroken {
                    Label(
                        "This model is no longer available. Sends will fail until it is changed.",
                        systemImage: "exclamationmark.triangle"
                    )
                    .font(.caption)
                    .foregroundStyle(Palette.attention)
                    Menu("Change model") {
                        ForEach(store.models) { option in
                            Button(option.name) {
                                Task { await store.updateBot(bot, fields: ["model": option.id]) }
                            }
                        }
                    }
                }
            }
            Section("System prompt") {
                Text(bot.systemPrompt.isEmpty ? "(none)" : bot.systemPrompt)
                    .textSelection(.enabled)
            }
            conversationSection
            Section("Info") {
                LabeledContent("Created", value: bot.createdAt.formatted())
                LabeledContent("Messages", value: "\(messageCount)")
                LabeledContent("ID", value: bot.id)
            }
        }
        .navigationTitle("Details")
        .task(id: bot.id) { await store.loadSegments(bot.id) }
    }

    // The chain of segments this bot's one conversation is stored as. Compaction
    // seals the open segment with a cumulative summary and opens a fresh one, so
    // the sealed entries read oldest-first as the bot's memory of itself.
    private var conversationSection: some View {
        Section("Conversation") {
            Button {
                Task { await store.compact(bot) }
            } label: {
                if compacting {
                    HStack(spacing: 6) {
                        ProgressView().controlSize(.small)
                        Text("Compacting…")
                    }
                } else {
                    Label("Compact", systemImage: "arrow.down.right.and.arrow.up.left")
                }
            }
            .disabled(compacting || messageCount == 0)

            if chain.isEmpty {
                Text("Never compacted.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                ForEach(chain.sorted { $0.index < $1.index }) { segment in
                    SegmentRow(segment: segment)
                }
            }
        }
    }
}

private struct SegmentRow: View {
    let segment: Segment

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack {
                Text("Segment \(segment.index + 1)")
                    .font(.caption.weight(.semibold))
                Spacer()
                Text(segment.sealed.map { "sealed \($0.formatted(date: .abbreviated, time: .shortened))" } ?? "open")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if let summary = segment.summary, !summary.isEmpty {
                Text(summary)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
        }
    }
}
