package slack

import "testing"

func TestAddress(t *testing.T) {
	if got := address("✅ done", ""); got != "✅ done" {
		t.Errorf("no mention: %q", got)
	}
	if got := address("✅ done", "U42"); got != "<@U42> ✅ done" {
		t.Errorf("mention: %q", got)
	}
}
