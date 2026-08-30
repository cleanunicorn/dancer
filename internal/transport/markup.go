package transport

import "regexp"

// Link renders a clickable label in dispatch's own markup: "<url|label>",
// which is exactly Slack's mrkdwn link — the transport most of dispatch's
// own lines are written for renders it with no help. The web UI's mrkdwn
// renderer understands the same form, and the terminal turns it into an
// OSC 8 hyperlink or flattens it (RenderLinks).
//
// It is for dispatch's own lines only. Agent text is Markdown
// (Outbound.Markdown) and goes to the transport untouched, so a "<…|…>"
// the agent happened to write is never treated as a link.
//
// With no url there is nothing to click and the label is returned alone,
// so a caller may hand over whatever it has.
func Link(url, label string) string {
	if url == "" || label == "" {
		return label
	}
	return "<" + url + "|" + label + ">"
}

// linkRE matches what Link writes. It insists on a scheme and a "|", so
// Slack's other angle-bracket forms — a "<@U123>" mention, a bare
// "<https://…>" autolink — are left alone.
var linkRE = regexp.MustCompile(`<(https?://[^|>\s]+)\|([^>\n]+)>`)

// RenderLinks rewrites every Link in text through render, for a transport
// that cannot show Slack's form as it stands.
func RenderLinks(text string, render func(url, label string) string) string {
	return linkRE.ReplaceAllStringFunc(text, func(m string) string {
		g := linkRE.FindStringSubmatch(m)
		return render(g[1], g[2])
	})
}
