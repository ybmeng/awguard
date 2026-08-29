package botnet

import (
	"testing"

	modelselector "stdtools/go/lib/modelSelector"
)

// TestRoundTrip is the "framework booted" finish condition: create a net, add a
// bot, append messages, and read the conversation back by BotID.
func TestRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}

	bot, err := s.CreateBot(net.ID, "Ada", "You are Ada, a helpful bot.", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	want := []struct{ role, content string }{
		{"user", "hello"},
		{"bot", "hi, I'm Ada"},
		{"user", "what can you do?"},
	}
	for _, m := range want {
		if _, err := s.AppendMessage(bot.ID, m.role, m.content); err != nil {
			t.Fatalf("append %q: %v", m.content, err)
		}
	}

	conv, err := s.Conversation(bot.ID)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(conv) != len(want) {
		t.Fatalf("got %d messages, want %d", len(conv), len(want))
	}
	for i, m := range conv {
		if m.Role != want[i].role || m.Content != want[i].content {
			t.Errorf("message %d = (%q, %q), want (%q, %q)", i, m.Role, m.Content, want[i].role, want[i].content)
		}
		if m.BotID != bot.ID {
			t.Errorf("message %d bot id = %q, want %q", i, m.BotID, bot.ID)
		}
		if m.SentAt.IsZero() {
			t.Errorf("message %d has zero SentAt", i)
		}
	}

	// Net membership is derived from the bots table.
	got, err := s.GetNet(net.ID)
	if err != nil {
		t.Fatalf("get net: %v", err)
	}
	if len(got.Bots) != 1 || got.Bots[0] != bot.ID {
		t.Errorf("net bots = %v, want [%s]", got.Bots, bot.ID)
	}

	// Bot round-trips by id with its model reference intact.
	gotBot, err := s.GetBot(bot.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if gotBot.Model != modelselector.DeepSeekV4.ID {
		t.Errorf("bot model = %q, want %q", gotBot.Model, modelselector.DeepSeekV4.ID)
	}
	if gotBot.SystemPrompt != "You are Ada, a helpful bot." {
		t.Errorf("bot system prompt = %q", gotBot.SystemPrompt)
	}
}

func TestGetBotNotFound(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.GetBot("bot_nope"); err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}
