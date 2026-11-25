// @vitest-environment node
// KIN-110: the colour tokens in styles.css meet WCAG 2.2 AA in both themes — 3:1 for borders and other non-text
// UI (1.4.11) against every surface they sit on or next to, 4.5:1 for body text (1.4.3). Telegram theme variables
// are not ours to check; the fallbacks after them are.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const css = readFileSync(fileURLToPath(new URL("../styles.css", import.meta.url)), "utf8").replace(
  /\/\*[\s\S]*?\*\//g,
  "",
);

// The custom properties of the first `:root { … }` block after `from` (light: the top level; dark: the one inside
// the prefers-color-scheme media query), each resolved to the hex colour it falls back to.
function tokens(from: number): Record<string, string> {
  const start = css.indexOf(":root", from);
  const body = css.slice(css.indexOf("{", start) + 1, css.indexOf("}", start));
  const out: Record<string, string> = {};
  for (const m of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    const hex = /#[0-9a-fA-F]{6}\b/.exec(m[2]!);
    if (hex) out[m[1]!] = hex[0];
  }
  return out;
}

function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

function ratio(a: string, b: string): number {
  const [lo, hi] = [luminance(a), luminance(b)].sort((x, y) => x - y);
  return (hi! + 0.05) / (lo! + 0.05);
}

const themes = {
  light: tokens(0),
  dark: tokens(css.indexOf("prefers-color-scheme: dark")),
};

describe.each(Object.entries(themes))("%s theme tokens", (_, t) => {
  it.each(["--bg", "--surface", "--text", "--warn-bg", "--warn-border"])("declares %s as a hex colour", (name) => {
    expect(t[name]).toMatch(/^#[0-9a-fA-F]{6}$/);
  });

  it.each(["--bg", "--surface", "--warn-bg"])("--warn-border has at least 3:1 against %s", (bg) => {
    expect(ratio(t["--warn-border"]!, t[bg]!)).toBeGreaterThanOrEqual(3);
  });

  it("--text on --warn-bg has at least 4.5:1", () => {
    expect(ratio(t["--text"]!, t["--warn-bg"]!)).toBeGreaterThanOrEqual(4.5);
  });
});
