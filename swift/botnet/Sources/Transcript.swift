// Transcript.swift — turns the server's flat message list into the shape the
// chat panel draws: consecutive same-speaker messages collapse into one turn,
// each message keeps its identity, and each paragraph becomes its own bubble.
// Pure data, no SwiftUI.
//
// Message boundaries survive the grouping on purpose. A turn is a visual block,
// but the id, status and error belong to a message, and the UI exposes them
// per message.

import Foundation

struct ChatTurn: Identifiable {
    let id: String                      // id of the turn's first message
    let role: Message.Role
    let messages: [TranscriptMessage]

    var bubbleCount: Int { messages.reduce(0) { $0 + $1.bubbles.count } }
    /// The last bubble in the turn, which is what scroll-to-bottom targets.
    var lastBubbleID: String? { messages.last?.bubbles.last?.id }
    /// The stranded message, if the model call for this turn failed.
    var failed: TranscriptMessage? { messages.first(where: \.didFail) }
}

struct TranscriptMessage: Identifiable {
    let id: String                      // the real "msg_" id, shown in the UI
    let bubbles: [TranscriptBubble]
    let status: Message.Status?
    let error: String?

    var didFail: Bool { status == .failed }
    var isAwaiting: Bool { status == .awaiting }

    /// What to show when the turn failed, since the server does not always
    /// attach a reason.
    var failureText: String? {
        guard didFail else { return nil }
        if let error, !error.isEmpty { return error }
        return "Couldn't get a reply."
    }
}

struct TranscriptBubble: Identifiable {
    let id: String                      // "<messageID>#<paragraphIndex>"
    let text: String
}

extension ChatTurn {
    static func build(from messages: [Message]) -> [ChatTurn] {
        var turns: [ChatTurn] = []
        var currentRole: Message.Role?
        var currentID = ""
        var current: [TranscriptMessage] = []

        func flush() {
            guard let role = currentRole, !current.isEmpty else { return }
            turns.append(ChatTurn(id: currentID, role: role, messages: current))
        }

        for message in messages {
            if message.role != currentRole {
                flush()
                currentRole = message.role
                currentID = message.id
                current = []
            }
            let bubbles = self.bubbles(of: message)
            // A message with no renderable text still matters when it failed or
            // is in flight, because that is the only thing the user can see.
            guard !bubbles.isEmpty || message.didFail || message.status == .awaiting else { continue }
            current.append(TranscriptMessage(
                id: message.id,
                bubbles: bubbles,
                status: message.status,
                error: message.error
            ))
        }
        flush()
        return turns
    }

    private static func bubbles(of message: Message) -> [TranscriptBubble] {
        paragraphs(of: message.content).enumerated().map { index, text in
            TranscriptBubble(id: "\(message.id)#\(index)", text: text)
        }
    }

    private static func paragraphs(of content: String) -> [String] {
        var out: [String] = []
        var buffer: [String] = []

        func close() {
            let joined = buffer.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
            if !joined.isEmpty { out.append(joined) }
            buffer = []
        }

        for line in content.components(separatedBy: .newlines) {
            if line.trimmingCharacters(in: .whitespaces).isEmpty {
                close()
            } else {
                buffer.append(line)
            }
        }
        close()
        return out
    }
}
