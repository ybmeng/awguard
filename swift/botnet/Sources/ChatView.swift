// ChatView.swift — the bot chat panel, plus the right-panel inspector with the
// details the chat was created with (system prompt, model, created, count).

import SwiftUI

struct ChatView: View {
    @EnvironmentObject var store: AppStore
    let bot: Bot

    @State private var draft = ""
    @State private var showDetails = false

    private var messages: [Message] { store.messages(for: bot.id) }
    private var pending: Bool { store.pendingBotIDs.contains(bot.id) }

    var body: some View {
        VStack(spacing: 0) {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 10) {
                        ForEach(messages) { msg in
                            MessageRow(message: msg)
                        }
                        if pending {
                            HStack {
                                ProgressView()
                                Text("thinking…").foregroundStyle(.secondary)
                            }
                            .id("pending")
                        }
                    }
                    .padding()
                }
                .onChange(of: messages.count) {
                    if let last = messages.last?.id {
                        withAnimation { proxy.scrollTo(last, anchor: .bottom) }
                    }
                }
            }
            Divider()
            HStack(spacing: 8) {
                TextField("Message \(bot.displayName)…", text: $draft, axis: .vertical)
                    .textFieldStyle(.roundedBorder)
                    .lineLimit(1...5)
                    .onSubmit(sendDraft)
                Button(action: sendDraft) {
                    Image(systemName: "arrow.up.circle.fill").font(.title2)
                }
                .disabled(pending || draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
            .padding()
        }
        .navigationTitle(bot.displayName)
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button { showDetails.toggle() } label: { Image(systemName: "info.circle") }
            }
        }
        .inspector(isPresented: $showDetails) {
            BotDetails(bot: bot, messageCount: messages.count)
        }
    }

    private func sendDraft() {
        let text = draft
        draft = ""
        Task { await store.send(text, to: bot) }
    }
}

private struct MessageRow: View {
    let message: Message

    var body: some View {
        HStack {
            if message.role == .user { Spacer(minLength: 40) }
            Text(message.content)
                .padding(10)
                .background(
                    message.role == .user
                        ? AnyShapeStyle(Color.accentColor.opacity(0.2))
                        : AnyShapeStyle(.quaternary),
                    in: RoundedRectangle(cornerRadius: 12)
                )
            if message.role != .user { Spacer(minLength: 40) }
        }
        .id(message.id)
    }
}

private struct BotDetails: View {
    let bot: Bot
    let messageCount: Int

    var body: some View {
        List {
            Section("Model") {
                Text(ModelOption.roster.first(where: { $0.id == bot.model })?.name ?? bot.model)
                Text(bot.model).font(.caption).foregroundStyle(.secondary)
            }
            Section("System prompt") {
                Text(bot.systemPrompt.isEmpty ? "(none)" : bot.systemPrompt)
            }
            Section("Info") {
                LabeledContent("Created", value: bot.createdAt.formatted())
                LabeledContent("Messages", value: "\(messageCount)")
                LabeledContent("ID", value: bot.id)
            }
        }
        .navigationTitle("Details")
    }
}
