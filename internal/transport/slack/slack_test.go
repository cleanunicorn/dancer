package slack

import "testing"

func TestAddress(t *testing.T) {
	if got := address("✅ done", ""); got != "✅ done" {
		t.Errorf("no mention: %q", got)
	}
	if got := address("", "U42"); got != "" {
		t.Errorf("nothing to say, still addressed: %q", got)
	}
	if got := address("✅ done", "U42"); got != "<@U42> ✅ done" {
		t.Errorf("mention: %q", got)
	}
	// A settled prompt drops the address again, and only that one.
	if got := unaddress(address("🔐 *Bash* wants to run <@U7>", "U42")); got != "🔐 *Bash* wants to run <@U7>" {
		t.Errorf("settled: %q", got)
	}
}

// Slack only makes a `<@U…>` when the writer picks the bot from the
// autocomplete. Typed by hand — on a phone, or with a name that is not
// the bot's handle — the address arrives as plain text, and a message
// that should have begun with "/" no longer does.
func TestStripMention(t *testing.T) {
	const id, name = "U0BOT", "dispatch"
	cases := []struct{ in, want string }{
		{"<@U0BOT> /compact", "/compact"},
		{"@dispatch /compact", "/compact"},
		{"@Dispatch /compact", "/compact"}, // Slack handles are case-insensitive
		{"@dispatch", ""},                  // the address and nothing else
		{"@dispatch  fix the build", "fix the build"},
		{"<@U0BOT> status", "status"},
		// Not an address: leave what the human is saying alone.
		{"@babel/core is broken", "@babel/core is broken"},
		{"@dispatcher is a different word", "@dispatcher is a different word"},
		{"ask @dispatch about it", "ask @dispatch about it"}, // not at the front
		{"/compact", "/compact"},
		{"fix the build", "fix the build"},
	}
	for _, c := range cases {
		if got := stripMention(c.in, id, name); got != c.want {
			t.Errorf("stripMention(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A transport that never learned its name still strips the token.
	if got := stripMention("<@U0BOT> hi", id, ""); got != "hi" {
		t.Errorf("without a bot name = %q", got)
	}
}
