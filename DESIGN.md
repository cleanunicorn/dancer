---
name: dispatch web UI
description: A flight-progress strip board for supervising coding agents; paper strips in an aluminium rack, lamps for state, ink for what humans write.
colors:
  # the console (HeroUI theme variables, light values; dark values in .impeccable/design.json)
  console-bg: "oklch(0.905 0.01 250)"
  console-fg: "oklch(0.23 0.014 250)"
  console-surface: "oklch(0.94 0.008 250)"
  console-surface-secondary: "oklch(0.88 0.01 250)"
  console-muted: "oklch(0.46 0.016 250)"
  console-border: "oklch(0.8 0.012 250)"
  console-field: "oklch(0.975 0.005 250)"
  console-link: "oklch(0.42 0.12 250)"
  # the rack
  rack: "oklch(0.835 0.012 250)"
  rack-ink: "oklch(0.3 0.016 250)"
  rack-rule: "oklch(0.75 0.012 250)"
  # paper
  paper-buff: "oklch(0.93 0.045 92)"
  paper-blue: "oklch(0.915 0.035 235)"
  paper-amber: "oklch(0.91 0.085 80)"
  paper-pink: "oklch(0.92 0.045 18)"
  paper-grey: "oklch(0.885 0.006 85)"
  # ink
  ink: "oklch(0.2 0.02 60)"
  ink-2: "oklch(0.42 0.035 70)"
  ink-rule: "oklch(0.2 0.02 60 / 0.16)"
  flag-run-ink: "oklch(0.4 0.12 150)"
  flag-fail-ink: "oklch(0.48 0.19 27)"
  # lamps
  lamp-wait: "oklch(0.8 0.17 75)"
  lamp-run: "oklch(0.74 0.18 150)"
  lamp-fail: "oklch(0.62 0.22 27)"
  lamp-off: "oklch(0.2 0.02 60 / 0.18)"
  # HeroUI semantic colours (buttons, toasts, sys-line tones)
  accent: "oklch(0.72 0.155 72)"
  accent-foreground: "oklch(0.2 0.03 70)"
  success: "oklch(0.66 0.16 150)"
  warning: "oklch(0.76 0.15 72)"
  danger: "oklch(0.58 0.2 27)"
  danger-foreground: "oklch(0.98 0.01 27)"
  focus: "oklch(0.55 0.16 72)"
typography:
  headline:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', sans-serif"
    fontSize: "15px"
    fontWeight: 600
    lineHeight: 1.25
  title:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', sans-serif"
    fontSize: "13px"
    fontWeight: 500
    lineHeight: 1.25
  body:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.55
  sys:
    fontFamily: "ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', sans-serif"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.45
  data:
    fontFamily: "ui-monospace, 'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', monospace"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.3
    fontVariation: "tabular-nums"
  gutter:
    fontFamily: "ui-monospace, 'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', monospace"
    fontSize: "11px"
    fontWeight: 400
    lineHeight: 1.2
    fontVariation: "tabular-nums"
  label:
    fontFamily: "ui-monospace, 'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', monospace"
    fontSize: "11px"
    fontWeight: 400
    lineHeight: 1.2
    letterSpacing: "0.06em"
  flag:
    fontFamily: "ui-monospace, 'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', monospace"
    fontSize: "10px"
    fontWeight: 400
    lineHeight: 1
    letterSpacing: "0.08em"
rounded:
  paper: "2px"
  chip: "3px"
  control: "0.2rem"
  lamp: "50%"
spacing:
  hair: "3px"
  xs: "0.25rem"
  sm: "0.5rem"
  md: "0.75rem"
  lg: "1rem"
  xl: "1.25rem"
  gutter: "4.25rem"
  rack: "19.5rem"
  column: "56rem"
components:
  strip:
    backgroundColor: "{colors.paper-buff}"
    textColor: "{colors.ink}"
    rounded: "{rounded.paper}"
    padding: "0.4rem 0.6rem 0.4rem 0.5rem"
  strip-slack:
    backgroundColor: "{colors.paper-blue}"
    textColor: "{colors.ink}"
  strip-wait:
    backgroundColor: "{colors.paper-amber}"
    textColor: "{colors.ink}"
  strip-fail:
    backgroundColor: "{colors.paper-pink}"
    textColor: "{colors.ink}"
  strip-closed:
    backgroundColor: "{colors.paper-grey}"
    textColor: "{colors.ink-2}"
  desk-strip:
    backgroundColor: "{colors.paper-buff}"
    textColor: "{colors.ink}"
    rounded: "{rounded.paper}"
    padding: "0.625rem 1.25rem"
  slip:
    backgroundColor: "{colors.paper-buff}"
    textColor: "{colors.ink}"
    rounded: "{rounded.paper}"
    padding: "0.45rem 0.7rem"
  prompt:
    backgroundColor: "{colors.paper-amber}"
    textColor: "{colors.ink}"
    rounded: "{rounded.paper}"
    padding: "0.6rem 0.8rem 0.7rem"
  prompt-settled:
    backgroundColor: "{colors.paper-grey}"
    textColor: "{colors.ink-2}"
  flag:
    textColor: "{colors.ink-2}"
    typography: "{typography.flag}"
    rounded: "{rounded.paper}"
    padding: "3px 4px 2px 5px"
  stamp:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.paper-buff}"
    typography: "{typography.flag}"
    rounded: "{rounded.paper}"
    padding: "3px 5px 2px"
  lamp:
    backgroundColor: "{colors.lamp-off}"
    rounded: "{rounded.lamp}"
    size: "8px"
  bay-label:
    textColor: "{colors.rack-ink}"
    typography: "{typography.label}"
    padding: "0.6rem 1rem 0.25rem"
  button-allow:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.accent-foreground}"
    typography: "{typography.flag}"
    rounded: "{rounded.control}"
  button-deny:
    backgroundColor: "{colors.danger}"
    textColor: "{colors.danger-foreground}"
    typography: "{typography.flag}"
    rounded: "{rounded.control}"
  button-choice:
    backgroundColor: "{colors.console-surface}"
    textColor: "{colors.console-fg}"
    rounded: "{rounded.control}"
  printer:
    backgroundColor: "{colors.console-surface}"
    padding: "0.75rem 1.25rem"
  printer-field:
    backgroundColor: "{colors.console-field}"
    textColor: "{colors.console-fg}"
    typography: "{typography.body}"
    rounded: "{rounded.control}"
  kbd:
    backgroundColor: "{colors.console-surface}"
    textColor: "{colors.console-fg}"
    rounded: "{rounded.chip}"
    padding: "0 4px"
---

# Design System: dispatch web UI

## Overview

**Creative North Star: "The Flight-Strip Board"**

Every thread is a paper flight-progress strip. The operator reads a rack of printed strips down the left edge and pulls one onto the desk; the open strip sits across the head of the desk with its fields printed on it, the log runs beneath it in a time-gutter grid, and the printer (the composer) sits along the bottom. The console is brushed aluminium in daylight and graphite-blue at night; the paper is always light and the ink is always dark, whichever the scheme. State is carried by a lamp (amber asks, green runs, red failed) and a four-letter printed flag, never by colour alone, and a strip that needs a human is lit and cocked out of the rack so it is found without hunting.

The world refuses the chat-app scaffold: no avatars, no bubbles, no sidebar list of conversations. Instead the material says who is speaking. Humans write on paper (strips, slips, prompts); the machine speaks on the desk (agent Markdown, dispatch's own lines in the rack's muted voice). Density is high and quiet: an all-day second-monitor surface that earns trust by being legible from across the room and calm up close.

Type follows the same split. Anything printed on a strip or measured (flags, field values, elapsed times, the time gutter, labels, the live line) is set in the system monospace; prose (titles, agent text, dispatch's lines, help) is the system sans. No fonts are bundled.

**Key Characteristics:**
- Paper on a console: warm buff and pale-blue strips lift off a cool grey-blue rack and desk.
- Lamp plus flag for every state; only WAIT, RUN and FAIL light a lamp.
- A cocked strip (translated 8px, rotated -0.4deg, lifted shadow) is the one dramatic gesture.
- Mono for printed data, sans for prose; 10-15px, tabular numerals throughout.
- Two shadows only: paper at rest, paper pulled.
- 2px corners on paper; perforated (dashed) left edge on every strip.

## Colors

A cool grey-blue console (hue 250) that recedes, warm paper (hues 80-92) that reads as the object, and three saturated lamps that are the only bright things on the board.

### Primary
- **Amber Lamp** (`{colors.lamp-wait}`): the lamp on a strip that is waiting for a human; pulses at 1.3s. The same hue is HeroUI's `--accent`, so the ALLOW button, focus ring and `@mention` callout wear the amber of the ask.
- **Amber Paper** (`{colors.paper-amber}`): the paper a waiting strip, the desk strip while waiting, and an open prompt are printed on. Amber paper and the amber lamp always appear together.

### Secondary
- **Green Lamp** (`{colors.lamp-run}`) and **Run Flag Ink** (`{colors.flag-run-ink}`): the agent is working, or the link is up. `{colors.success}` tints a dispatch line that begins with a check mark.
- **Red Lamp** (`{colors.lamp-fail}`), **Fail Flag Ink** (`{colors.flag-fail-ink}`), **Pink Paper** (`{colors.paper-pink}`): a failed task. `{colors.danger}` is the DENY button and a dispatch line that begins with a cross.

### Neutral
- **Console** (`{colors.console-bg}` / `{colors.console-fg}`): the desk. Agent Markdown and dispatch's lines sit directly on it; `{colors.console-muted}` is the gutter, the speaker label and dispatch's ordinary voice.
- **Console Surface** (`{colors.console-surface}`, `{colors.console-surface-secondary}`): the printer bar, choice buttons, code headers; `{colors.console-field}` is the textarea and fenced code on the desk.
- **Rack** (`{colors.rack}` / `{colors.rack-ink}` / `{colors.rack-rule}`): the aluminium the strips sit in, its stamped labels, and the 1px rules between bays. The rack carries a faint 8px horizontal grain (`repeating-linear-gradient`, 3.5% black in light, 2.5% white in dark).
- **Buff Paper** (`{colors.paper-buff}`): the default strip, slip and desk strip (threads hosted by the web transport). **Blue Paper** (`{colors.paper-blue}`): anything hosted by or written from Slack. **Grey Paper** (`{colors.paper-grey}`): a closed thread or a settled prompt; it casts no shadow.
- **Ink** (`{colors.ink}` / `{colors.ink-2}` / `{colors.ink-rule}`): everything printed on paper. `ink-2` is the second line of a strip, field keys and settled text; `ink-rule` is the perforation and the dashed rule between a strip and its prompt.
- **Lamp Off** (`{colors.lamp-off}`): the unlit lamp every strip still carries (DONE, IDLE, QUED, INTR, CNCL, CLSD, NEW).

### Named Rules
**The Lamp Rule.** State is a lamp plus a printed flag. Only WAIT, RUN and FAIL light a lamp (amber, green, red); every other state shows the unlit lamp and its four-letter flag. Colour is never the sole carrier.

**The Paper Is Always Lit Rule.** Paper stays light and ink stays dark in both colour schemes; the dark scheme darkens the console and the rack, lowers paper by ~0.03 L, and leaves every `ink` token untouched.

**The Paper/Desk Rule.** What a human writes or must answer is printed on paper; what the machine says sits on the desk. Agent Markdown and dispatch's own lines never get a paper background.

**The Host Colour Rule.** Blue paper means Slack hosts or wrote it; buff means the web. The host colour yields to state: a waiting strip is amber and a failed strip is pink whoever hosts it.

## Typography

**Display Font:** none; the largest type on the board is the 15px desk heading.
**Body Font:** system sans (`ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", sans-serif`)
**Label/Mono Font:** system mono (`ui-monospace, "SF Mono", Menlo, Consolas, "DejaVu Sans Mono", monospace`), exposed to Tailwind as `font-strip`

**Character:** Printed, not typeset. Mono carries every value a controller would read off a strip; sans carries prose. The whole ramp sits between 10px and 15px, tabular numerals everywhere a number can change.

### Hierarchy
- **Headline** (600, 15px, 1.25): the pulled strip's title on the desk and the wordmark in the rack. Truncates on one line.
- **Title** (500, 13px, 1.25; 700 while the thread is fresh/unread): a strip's title in the rack. One line, ellipsis.
- **Body** (400, 14px, 1.55): agent Markdown and human slips; measure 72ch. Markdown headings step 1.2em / 1.1em / 1.02em at 600, never larger.
- **Sys** (400, 13px, 1.45): dispatch's own lines, in `console-muted`; measure 80ch; tinted by leading glyph (ok/bad/warn) and raised to `console-fg` when it mentions the signed-in user.
- **Data** (mono 400, 12px, 1.3, tabular): field values on the desk strip, the live status line, the prompt text riding on the strip, file names.
- **Gutter** (mono 400, 11px, 1.2, tabular): the time column, a strip's second line and age, the speaker line.
- **Label** (mono 400, 11px, 0.06em, uppercase): bay labels, the speaker label, `@mention`. Field keys on the desk strip and form labels drop to 10px at 0.1em.
- **Flag** (mono, 10px, 0.08em, line-height 1; 700 for WAIT and FAIL): the four-letter flag, the unread stamp, the ALLOW/DENY/SEND button faces (uppercase, `tracking-wider`).

### Named Rules
**The Printed Data Rule.** If it is a value a controller reads off a strip (flag, field, time, count, tool name), it is mono with tabular numerals. If it is a sentence, it is sans.

**The Four-Letter Flag Rule.** A state is printed as exactly four uppercase letters in a 1px box: WAIT RUN FAIL CLSD DONE IDLE QUED INTR CNCL NEW. New states join this set; nothing else is boxed like a flag except the SIGN IN card's flag.

## Layout

Two panes, no header. The rack is a fixed 19.5rem column down the left edge (`{spacing.rack}`), scrolling independently, with the wordmark at top and the account/link line at the bottom; the desk fills the rest. Below 768px the rack slides off-screen (`translate-x`) behind a hamburger on the desk strip and a backdrop; that is the only breakpoint.

The rack is stacked bays. A "needs you" bay sits on top whenever any strip is waiting; then one bay per channel, grouped by transport with the web first. Strips stack with a 3px gap (`{spacing.hair}`) and 0.5rem side margins; a bay label is 0.6rem above, 0.25rem below, and rules out to the right edge. Strips sort by urgency (wait, run/queued, fail, then recency) and a bay shows at most 50.

The desk is three bands. The desk strip is inset 0.75rem (1.25rem on desktop) from the edges; its fields are fixed widths on desktop (channel 9rem, started-by 7rem, elapsed 5.5rem) and wrap under the title on mobile; an open prompt adds a second row under a dashed rule. The log is a two-column grid, 4.25rem time gutter plus a fluid column (`{spacing.gutter}`), 0.75rem column gap, 0.25rem row gap, centred at a 56rem maximum (`{spacing.column}`); on mobile the gutter disappears and the time moves into the speaker line. The printer is a full-width bar with the same 56rem inner width, 0.75rem vertical padding.

Spacing is a 4px rhythm: 0.25 / 0.5 / 0.75 / 1 / 1.25rem, with the 3px hair used only for strip stacking and the flag box.

## Elevation & Depth

A hybrid: tonal layering for the console (rack darker than desk, surface lighter than desk, field lightest) and real shadows for paper only. Paper is the one thing with thickness. Nothing on the console casts a shadow; depth there comes from the 1px `console-border` rule and the rack's grain.

### Shadow Vocabulary
- **Paper at rest** (`box-shadow: 0 1px 1px oklch(0 0 0 / 0.12), 0 2px 6px -2px oklch(0 0 0 / 0.18)`): a racked strip, a slip, a settled prompt, a closed desk strip.
- **Paper pulled** (`box-shadow: 0 2px 2px oklch(0 0 0 / 0.14), 0 8px 16px -6px oklch(0 0 0 / 0.3)`): the current strip, a cocked waiting strip, the desk strip, an open prompt, the toast. The dark scheme deepens both (0.35/0.5 and 0.4/0.6 alpha).
- **Lamp glow** (`0 0 0 1px <ring>, 0 0 6-8px 1-2px <glow>`): only on a lit lamp, in the lamp's own hue; the unlit lamp has a 1px inset shadow instead.

### Named Rules
**The Two Shadows Rule.** Paper is at rest or pulled; there is no third elevation. A strip rises by changing shadow and translating 8px, never by scaling or adding a border.

**The Cocked Strip Rule.** A waiting strip is pulled 8px out of the rack and rotated -0.4deg (10px, no rotation, when it is also the current one). Hover nudges a strip 3px; nothing else on the board moves sideways.

## Shapes

Paper has 2px corners (`{rounded.paper}`) and a perforated left edge: a 3px (4px on the desk strip) strip inside the left edge with a 1px dashed `ink-rule` border, the edge it was torn off along. Flags are 1px boxes in `currentColor` with 2px corners (dashed border for CLSD). Lamps are 8px circles. Code, kbd and small chips use 3px (`{rounded.chip}`); HeroUI controls (buttons, fields, modals) use the theme radius 0.2rem (`{rounded.control}`). A decision stamp, the answer printed on a settled prompt, is a 1.5px ink box rotated -1.5deg. Nothing is pill-shaped and nothing is larger than 3.2px in radius except a lamp.

Motion is paper motion only: a strip pulled onto the desk drops in over 220ms (`translateY(-6px)`, opacity 0.6 to 1); strip hover/cock transitions run 180ms on `cubic-bezier(0.2, 0.8, 0.2, 1)`; the waiting lamp pulses brightness 1 to 0.7 over 1.3s. All of it stops under `prefers-reduced-motion`.

## Components

### Strip (rack)
- **Character:** a printed paper card with a lamp, a title, a second printed line, a flag and an age.
- **Grid:** 14px lamp column, fluid text, auto flag column; two rows. Title (sans 13px) over line (mono 11px, `ink-2`); flag slot over age, right-aligned.
- **Paper by host and state:** buff by default, blue when Slack hosts, amber when waiting, pink when failed, grey (no shadow, 0.85 opacity, `ink-2` text) when closed.
- **States:** hover translate 3px; current translate 8px with the pulled shadow; waiting cocked per the Cocked Strip Rule; focus-visible adds a 2px console gap and a 2px `focus` ring outside the paper shadow.
- **Unread:** a stamp, ink background with the strip's paper as text, "N NEW" in flag type, to the left of the flag.

### Desk strip
- **Character:** the same paper at full width across the head of the desk, printed with fields.
- **Fields:** key (mono 10px uppercase 0.1em `ink-2`) over value (mono 12px tabular `ink`): channel, started by, elapsed (ticking every second while RUN/WAIT; "updated N ago" otherwise), state (the flag). Title at 15px 600 with the lamp beside it.
- **While waiting:** the open prompt's text rides on the strip in a second row under a dashed `ink-rule`, with its ALLOW/DENY (or options) inline, so the answer is always at hand.
- **Also used for:** the login card, the toast, and the draft "New strip in #channel" header, all with `data-state="new"` (buff, unlit lamp, NEW flag).

### Slip (human line)
- **Style:** buff paper (blue when written via Slack), 2px corners, paper-at-rest shadow, 0.45rem 0.7rem padding, max 72ch, preformatted wrapping. Speaker line in `ink-2` with the name in `ink`. Links in `ink`, underlined. Code on paper is inked, not lit: 7% black background, `ink-rule` border.

### Prompt
- **Style:** amber paper, pulled shadow, lamp lit amber, speaker "dispatch" or "@user". Text in mrkdwn; then the choices.
- **Choices:** ALLOW is the primary button (`accent` amber, dark text), DENY is the danger button, any other choice or an option is a secondary button with a 25% ink border on `console-surface`; free-text answers get a field plus an uppercase ANSWER button. Button faces are mono uppercase `tracking-wider`.
- **Settled:** grey paper, rest shadow, `ink-2` text, lamp off; the answer appears as a decision stamp ("ALLOW" rotated -1.5deg) followed by who decided, via which transport, and the clock. A prompt answered elsewhere reads "settled elsewhere".

### Agent text and dispatch lines (on the desk)
- **Agent:** speaker line "AGENT", then Markdown at body size in a 72ch measure. Fences sit on `console-field` with a `console-border` rule and 3px corners at 12.5px; inline code is mono at 0.92em on a 9% currentColor tint; tables are bordered with `console-surface-secondary` headers; blockquotes are a 1px left rule in muted.
- **Sys:** 13px sans in `console-muted`, 80ch, no background. Tone follows the leading glyph dispatch already emits: check = `success`, cross = `danger`, pause/warn/stop/recycle = `warning`; a line addressed to the signed-in user is `console-fg` with a mono uppercase amber `@name` in front.
- **Live line:** mono 12px `console-fg` with a green lamp, truncating on one line, at the foot of the log.

### Lamp and flag
- **Lamp:** 8px circle. Off: `lamp-off` with a 1px inset shadow. Wait: `lamp-wait`, ringed and glowing amber, pulsing. Run: `lamp-run`, ringed and glowing green. Fail: `lamp-fail`, ringed and glowing red. Also the link indicator at the foot of the rack (green up, red down) and the "needs you" bay label.
- **Flag:** four letters, mono 10px 0.08em, 1px box in `currentColor`, 3/4/2/5px padding. Default `ink-2`; WAIT `ink` 700; RUN `flag-run-ink`; FAIL `flag-fail-ink` 700; CLSD dashed. On the console (help dialog) it takes `console-fg`.

### Bay label
- **Style:** mono 11px uppercase 0.06em in `rack-ink`, 0.6rem 1rem 0.25rem, a 1px `rack-rule` running to the right edge; the "needs you" bay carries a lit amber lamp and a count; a channel bay carries a ghost "+" icon button. An empty bay prints "empty bay" in the same mono.

### Printer (composer) and fields
- **Bar:** `console-surface` with a 1px `console-border` top rule; inner width 56rem.
- **Field:** HeroUI textarea on `console-field` with a 1px `console-border`-family stroke, 0.2rem radius, 14px sans, auto-growing from 2.5rem to 12rem; SEND is the primary (amber) button in flag type. Login and modal fields are the same HeroUI field; login labels are mono 10px uppercase 0.1em `ink-2` because they sit on paper.
- **Focus:** HeroUI's ring in `focus` (amber).

### Kbd
- **Style:** mono 10.5px on `console-surface`, 1px `console-border` with a 2px bottom, 3px corners, 0 4px padding.

### The mark
dispatch's mark is a strip with its lamp lit: a 17x10 buff rectangle with a 1px ink stroke and 1px corner, a dashed perforation at x=4.5, an amber lamp at (8.5,10), and two ink lines of printed text. It is drawn from the live tokens in the UI (`var(--paper)`, `var(--ink)`, `var(--lamp-wait)`) and with fixed hex in the favicon. It appears at 18px in the rack, 22px on the login card, 40px on the empty desk.

## Do's and Don'ts

### Do:
- **Do** put a lamp and a four-letter flag on anything that has a state; light the lamp only for WAIT, RUN and FAIL.
- **Do** print what humans write or must answer on paper (buff, blue for Slack, amber while asking, pink when failed, grey once settled or closed), and leave what the machine says on the desk.
- **Do** keep paper light and ink dark in both colour schemes; change only the console, the rack and the shadows for dark.
- **Do** set printed data (flags, fields, times, counts, tool names, button faces) in the system mono with tabular numerals, and prose in the system sans.
- **Do** use the two paper shadows only: at rest for racked and settled paper, pulled for current, cocked, desk and open-prompt paper.
- **Do** give every strip its perforated left edge (1px dashed `ink-rule`) and 2px corners.
- **Do** keep the answer on the strip: while a thread waits, the open prompt's choices ride on the desk strip.
- **Do** keep the ramp between 10px and 15px and measures at 72ch (prose on paper or agent text) and 80ch (dispatch lines).

### Don't:
- **Don't** draw avatars, chat bubbles, or a plain list of conversations; the rack of strips and the desk are the scaffold.
- **Don't** let colour carry state alone; a tinted line or paper always has its lamp, flag, or leading glyph.
- **Don't** give agent Markdown or dispatch's own lines a paper background or shadow.
- **Don't** add a third elevation, a border-on-hover, or scale transforms; paper rises by shadow and an 8px translate only.
- **Don't** round anything past 3px except HeroUI's 0.2rem controls and the 8px lamp; no pills.
- **Don't** rotate anything except the cocked waiting strip (-0.4deg) and the decision stamp (-1.5deg).
- **Don't** bundle a webfont or introduce a third family; the system sans and system mono stacks are the type.
- **Don't** invent a state name that is not four uppercase letters, and don't print a flag without its box.
- **Don't** let the blue host paper override a waiting (amber) or failed (pink) strip.
