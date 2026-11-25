import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

/*
 * Markdown-lite for untrusted AI text (KIN-44). Only React text nodes and a fixed set of elements are produced:
 * paragraphs with line breaks, bullet and numbered lists, `inline code`, **bold** and ``` fenced blocks.
 * There is no dangerouslySetInnerHTML and no links or images: HTML, markdown links and URLs stay plain text.
 */

type Block =
  | { kind: "p"; lines: string[] }
  | { kind: "ul" | "ol"; items: string[] }
  | { kind: "code"; lang: string; text: string };

const bullet = /^\s*[-*•]\s+(.*)$/;
const numbered = /^\s*\d{1,3}[.)]\s+(.*)$/;
const fence = /^\s*```\s*([\w-]*)\s*$/;
const heading = /^\s*#{1,6}\s+(.*)$/;

export function parseBlocks(text: string): Block[] {
  const out: Block[] = [];
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i] ?? "";
    const f = fence.exec(line);
    if (f) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !/^\s*```\s*$/.test(lines[i] ?? "")) body.push(lines[i++] ?? "");
      i++; // the closing fence (or the end of an unclosed block)
      out.push({ kind: "code", lang: (f[1] ?? "").toLowerCase(), text: body.join("\n") });
      continue;
    }
    if (line.trim() === "") {
      i++;
      continue;
    }
    const list = bullet.test(line) ? (["ul", bullet] as const) : numbered.test(line) ? (["ol", numbered] as const) : undefined;
    if (list) {
      const [kind, re] = list;
      const items: string[] = [];
      while (i < lines.length && re.test(lines[i] ?? "")) items.push(re.exec(lines[i++] ?? "")?.[1] ?? "");
      out.push({ kind, items });
      continue;
    }
    const para: string[] = [];
    while (i < lines.length) {
      const l = lines[i] ?? "";
      if (l.trim() === "" || fence.test(l) || bullet.test(l) || numbered.test(l)) break;
      para.push(heading.exec(l)?.[1] ?? l);
      i++;
    }
    if (para.length > 0) out.push({ kind: "p", lines: para });
  }
  return out;
}

/** `code` and **bold** inside one line; everything else is text. */
function inline(text: string): ReactNode[] {
  return text.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*)/g).map((part, i) => {
    if (part.length > 2 && part.startsWith("`") && part.endsWith("`")) return <code key={i}>{part.slice(1, -1)}</code>;
    if (part.length > 4 && part.startsWith("**") && part.endsWith("**")) return <strong key={i}>{part.slice(2, -2)}</strong>;
    return part;
  });
}

export function RichText({ text }: { text: string }) {
  const { t } = useTranslation();
  return (
    <div className="rich-text">
      {parseBlocks(text).map((b, i) => {
        switch (b.kind) {
          case "code": {
            const pre = (
              <pre>
                <code>{b.text}</code>
              </pre>
            );
            return b.lang === "json" ? (
              <details key={i} className="rich-plan">
                <summary>{t("planDiscussion.planJson")}</summary>
                {pre}
              </details>
            ) : (
              <div key={i}>{pre}</div>
            );
          }
          case "ul":
          case "ol": {
            const List = b.kind;
            return (
              <List key={i}>
                {b.items.map((it, j) => (
                  <li key={j}>{inline(it)}</li>
                ))}
              </List>
            );
          }
          default:
            return (
              <p key={i}>
                {b.lines.map((l, j) => (
                  <span key={j}>
                    {j > 0 && <br />}
                    {inline(l)}
                  </span>
                ))}
              </p>
            );
        }
      })}
    </div>
  );
}
