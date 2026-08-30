package agent

import "testing"

// TestKinds: the kinds list is what config validation and the wizard show,
// so every constant is in it, in display order, and nothing else is.
func TestKinds(t *testing.T) {
	kinds := Kinds()
	if len(kinds) != 3 || kinds[0] != KindClaude || kinds[1] != KindCodex || kinds[2] != KindOpenCode {
		t.Fatalf("Kinds() = %v", kinds)
	}
	for _, k := range kinds {
		if !k.Valid() {
			t.Errorf("%q is listed but not valid", k)
		}
	}
	for _, k := range []Kind{"", "gemini", "Claude"} {
		if k.Valid() {
			t.Errorf("%q is valid", k)
		}
	}
}
