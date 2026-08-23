// Renders dancer's own lines: Slack-style mrkdwn (*bold*, _italic_,
// `code`, ```fences```), links, and a leading @mention. Agent text is
// Markdown and goes through react-markdown instead (see Message.tsx).
import type { ReactNode } from "react";

const URL_RE = /(https?:\/\/[^\s<)]+)/g;

function inline(text: string, key: number): ReactNode[] {
  const out: ReactNode[] = [];
  // tokens: `code`, *bold*, _italic_, urls, text
  const re = /(`[^`\n]+`)|((?:^|(?<=[\s(]))\*[^*\n]+\*(?=[\s).,:;!?]|$))|((?:^|(?<=[\s(]))_[^_\n]+_(?=[\s).,:;!?]|$))|(https?:\/\/[^\s<)]+)/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const k = `${key}-${i++}`;
    if (m[1]) out.push(<code key={k}>{m[1].slice(1, -1)}</code>);
    else if (m[2]) out.push(<b key={k}>{m[2].slice(1, -1)}</b>);
    else if (m[3]) out.push(<i key={k}>{m[3].slice(1, -1)}</i>);
    else if (m[4])
      out.push(
        <a key={k} href={m[4]} target="_blank" rel="noopener" className="text-link underline">
          {m[4]}
        </a>,
      );
    last = re.lastIndex;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function Mrkdwn({ text, mention }: { text: string; mention?: string }) {
  const parts = text.split("```");
  const nodes: ReactNode[] = [];
  parts.forEach((part, i) => {
    if (i % 2) {
      nodes.push(<pre key={i}>{part.replace(/^\n/, "")}</pre>);
      return;
    }
    const lines = part.split("\n");
    lines.forEach((line, j) => {
      if (j > 0) nodes.push(<br key={`${i}-br-${j}`} />);
      nodes.push(...inline(line, i * 1000 + j));
    });
  });
  return (
    <span className="mrkdwn">
      {mention ? <span className="text-accent font-medium">@{mention} </span> : null}
      {nodes}
    </span>
  );
}

export function linkify(text: string): ReactNode[] {
  return text.split(URL_RE).map((part, i) =>
    /^https?:\/\//.test(part) ? (
      <a key={i} href={part} target="_blank" rel="noopener" className="underline">
        {part}
      </a>
    ) : (
      part
    ),
  );
}
