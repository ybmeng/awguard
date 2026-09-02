// ChatScreen.swift — one bot's conversation on the phone: the bubble
// transcript and the composer under it. The Mac's ChatView ported to a screen
// with no pointer: there is no hover, so the per-message id line is a tap
// rather than something that appears under the cursor, and no header row,
// because the navigation bar already carries the bot's name.
//
// The grouping is the shared one (ChatTurn.build). The markdown treatment is
// the shared rule. Nothing here reimplements either.

import SwiftUI

struct ChatScreen: View {
    @EnvironmentObject var store: AppStore
    let botID: String

    @State private var draft = ""

    /// Resolved on every render, never captured: a rename or a refetch has to
    /// reach an open conversation.
    private var bot: Bot? { store.bots.first { $0.id == botID } }

    private var messages: [Message] { store.messages(for: botID) }
    private var turns: [ChatTurn] { ChatTurn.build(from: messages) }
    private var pending: Bool { store.pendingBotIDs.contains(botID) }
    private var canSend: Bool {
        !pending && !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        Group {
            if let bot {
                conversation(with: bot)
            } else {
                VanishedRow(noun: "bot")
            }
        }
        .background(Palette.chrome)
        .navigationTitle(bot?.displayName ?? "Bot")
        .navigationBarTitleDisplayMode(.inline)
        // A bot's conversation can have moved on since the list was fetched,
        // and marking it read is the visit itself.
        .task(id: botID) {
            await store.loadConversation(botID)
            if let bot { await store.markRead(bot) }
        }
        // The store outlives this screen and keeps the draft, so leaving mid
        // sentence and coming back lands on the same unsent text.
        .onAppear { draft = store.composerDrafts[botID] ?? "" }
        .onDisappear { saveDraft(draft) }
    }

    private func conversation(with bot: Bot) -> some View {
        GeometryReader { geo in
            let content = max(0, geo.size.width - 2 * Metric.phoneHPad)
            let gutter = content * (1 - Metric.phoneBubbleWidthFraction)
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(turns.enumerated()), id: \.element.id) { index, turn in
                            TurnView(turn: turn, gutter: gutter, botID: botID)
                                .padding(.top, index == 0 ? 0 : Metric.turnGap)
                        }
                        if pending {
                            ThinkingBubble()
                                .padding(.top, turns.isEmpty ? 0 : Metric.turnGap)
                                .id("pending")
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, Metric.phoneHPad)
                    .padding(.vertical, Metric.phoneVPad)
                }
                // Opens at the newest turn and stays pinned while the reader is
                // at the bottom; scrolling up unpins, as a chat should.
                .defaultScrollAnchor(.bottom)
                .refreshable { await store.loadConversation(botID) }
                // The explicit scroll covers the reader who scrolled up and then
                // sent: the pin is off, but their own send must snap back down.
                // Pending is a trigger too — the thinking bubble appears without
                // changing the message count.
                .onChange(of: messages.count) { scrollToNewest(proxy) }
                .onChange(of: store.pendingBotIDs) { scrollToNewest(proxy) }
                // The composer is an inset, not scroll content: it has to sit
                // above the keyboard and clear of the home indicator, and the
                // transcript has to scroll behind neither.
                .safeAreaInset(edge: .bottom) { composer(for: bot) }
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

    private func composer(for bot: Bot) -> some View {
        HStack(alignment: .bottom, spacing: Metric.phoneRowGap) {
            TextField("Message \(bot.displayName)…", text: $draft, axis: .vertical)
                .textFieldStyle(.plain)
                .font(TypeScale.phoneComposer)
                .foregroundStyle(Palette.primaryText)
                .lineLimit(1...6)
                .padding(.vertical, Metric.phoneTightGap)

            Button { send(to: bot) } label: {
                Image(systemName: "arrow.up")
                    .foregroundStyle(canSend ? Palette.userBubbleText : Palette.secondaryText)
                    .frame(width: Metric.phoneComposerCircle, height: Metric.phoneComposerCircle)
                    .background(canSend ? Palette.userBubble : Palette.fieldFill, in: Circle())
            }
            .buttonStyle(.plain)
            .disabled(!canSend)
            .accessibilityLabel("Send")
        }
        .padding(.horizontal, Metric.phoneRowGap)
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
        .background(Palette.chrome)
    }

    private func send(to bot: Bot) {
        let text = draft
        draft = ""
        store.composerDrafts[botID] = nil
        Task { await store.send(text, to: bot) }
    }

    // Whitespace-only reads as "nothing typed" and removes the key; anything
    // else is stored verbatim — trimming here would eat text the user is
    // mid-way through composing.
    private func saveDraft(_ text: String) {
        store.composerDrafts[botID] =
            text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? nil : text
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

// One message: its paragraph bubbles, then the quiet id line. On the Mac that
// line brightens under the pointer; a phone has none, so it is simply a tappable
// line held back at rest and opened by a tap.
private struct MessageView: View {
    let message: TranscriptMessage
    let isUser: Bool
    let botID: String

    @EnvironmentObject private var store: AppStore
    @State private var expanded = false

    var body: some View {
        VStack(alignment: isUser ? .trailing : .leading, spacing: Metric.phoneTightGap) {
            ForEach(message.bubbles) { bubble in
                bubbleText(bubble.text)
                    .font(TypeScale.phoneMessage)
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
                HStack(spacing: Metric.phoneTightGap) {
                    Image(systemName: "exclamationmark.circle")
                    Text(failure)
                    Button("Retry") { Task { await store.retry(message.id, on: botID) } }
                        .buttonStyle(.plain)
                        .underline()
                }
                .font(TypeScale.phoneRowMeta)
                .foregroundStyle(Palette.attention)
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
        VStack(alignment: isUser ? .trailing : .leading, spacing: 1) {
            Button {
                expanded.toggle()
            } label: {
                Text(message.isAwaiting ? "sending…" : message.id)
                    .font(TypeScale.phoneMessageMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .lineLimit(1)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            // Always present, since the id is the handle for everything that
            // will accumulate here. Held back at rest so a column of ids never
            // competes with the conversation.
            .opacity(expanded || message.didFail ? 1 : 0.45)

            if expanded, let status = message.status?.rawValue {
                Text("status \(status)")
                    .font(TypeScale.phoneMessageMeta)
                    .foregroundStyle(Palette.secondaryText)
                    .textSelection(.enabled)
            }
        }
        .animation(.easeOut(duration: 0.12), value: expanded)
    }
}

private struct ThinkingBubble: View {
    var body: some View {
        HStack(spacing: 0) {
            HStack(spacing: Metric.phoneTightGap) {
                ProgressView()
                Text("thinking…")
                    .font(TypeScale.phoneMessage)
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
