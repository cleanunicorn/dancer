package transport

import "testing"

// TestLink: dispatch's own link markup, and the two things it must not
// swallow — Slack's mention, and a bare URL.
func TestLink(t *testing.T) {
	for _, tc := range []struct{ url, label, want string }{
		{"https://github.com/o/r/pull/51", "#51", "<https://github.com/o/r/pull/51|#51>"},
		{"", "#51", "#51"},      // nothing to click
		{"https://x/y", "", ""}, // nothing to click on
	} {
		if got := Link(tc.url, tc.label); got != tc.want {
			t.Errorf("Link(%q, %q) = %q, want %q", tc.url, tc.label, got, tc.want)
		}
	}
}

func TestRenderLinks(t *testing.T) {
	flat := func(url, label string) string { return label + " " + url }
	for _, tc := range []struct{ in, want string }{
		{
			"🔀 <https://github.com/o/r/pull/51|#51> · for <https://github.com/o/r/issues/47|#47>",
			"🔀 #51 https://github.com/o/r/pull/51 · for #47 https://github.com/o/r/issues/47",
		},
		// A mention has no scheme and no "|"; a bare URL is not wrapped.
		// Neither is dispatch's markup, and neither may be touched.
		{"<@U123> look at https://example.com/x", "<@U123> look at https://example.com/x"},
		{"no links here", "no links here"},
	} {
		if got := RenderLinks(tc.in, flat); got != tc.want {
			t.Errorf("RenderLinks(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
