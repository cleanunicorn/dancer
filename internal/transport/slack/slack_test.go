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
