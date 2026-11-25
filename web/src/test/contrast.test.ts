// @vitest-environment node
// KIN-110, KIN-111: every foreground/background colour pair the stylesheets use meets WCAG 2.2 AA in both themes —
// 4.5:1 for text (1.4.3), 3:1 for borders, focus rings, chart lines and other non-text UI (1.4.11) — and no
// stylesheet bypasses the theme with a hard-coded colour. axe cannot check contrast in jsdom (no layout), so this
// is the real guard. Telegram theme variables are not ours to check; the fallbacks after them are.
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

const srcDir = fileURLToPath(new URL("..", import.meta.url));
const stripComments = (s: string) => s.replace(/\/\*[\s\S]*?\*\//g, "");

const css = stripComments(readFileSync(`${srcDir}/styles.css`, "utf8"));
const featureSheets = readdirSync(`${srcDir}/features`, { withFileTypes: true })
  .filter((d) => d.isDirectory())
  .flatMap((d) =>
    readdirSync(`${srcDir}/features/${d.name}`)
      .filter((f) => f.endsWith(".css"))
      .map((f) => `features/${d.name}/${f}`),
  );
const sheets: Record<string, string> = {
  "styles.css": css,
  ...Object.fromEntries(
    featureSheets.map((f) => [f, stripComments(readFileSync(`${srcDir}/${f}`, "utf8"))]),
  ),
};

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

// Page surfaces content sits on: the page, cards/topbar/code blocks, and warn cards, badges and banners.
const SURFACES = ["--bg", "--surface", "--warn-bg"];
const TEXT = 4.5;
const UI = 3;

// [foreground, backgrounds, minimum ratio] for every pair the stylesheets use.
const PAIRS: [string, string[], number][] = [
  ["--text", SURFACES, TEXT], // body, inputs, buttons, badges, maintenance banner
  ["--muted", SURFACES, TEXT], // hints, counters, log events, chart labels, muted text in warn cards
  ["--link", SURFACES, TEXT], // links, also inside the maintenance banner
  ["--danger", ["--bg", "--surface"], TEXT], // field errors, stderr lines, over-limit counter
  ["--bg", ["--text"], TEXT], // .notice toast
  ["--accent-text", ["--accent"], TEXT], // primary buttons, skip link
  ["--danger-text", ["--danger"], TEXT], // danger buttons, critical badges
  ["--focus", SURFACES, UI], // focus ring
  ["--accent", SURFACES, UI], // primary button fill, current nav/revision/segment, chart series
  ["--danger", SURFACES, UI], // invalid inputs, failed-state badge borders, critical threshold
  ["--warn-border", SURFACES, UI], // warn badges/cards, warning threshold
  ["--border-strong", SURFACES, UI], // input, button and segmented control boundaries, chart axes
];

// Colour tokens with no contrast requirement, and why.
const DECORATIVE: Record<string, string> = {
  "--border": "dividers and card/table outlines next to a contrasting fill or text; never a control's only boundary",
};

describe.each(Object.entries(themes))("%s theme", (_, t) => {
  const cases = PAIRS.flatMap(([fg, bgs, min]) => bgs.map((bg) => [fg, bg, min] as const));

  it.each(cases)("%s on %s has at least %s:1", (fg, bg, min) => {
    expect(t[fg]).toMatch(/^#[0-9a-fA-F]{6}$/);
    expect(t[bg]).toMatch(/^#[0-9a-fA-F]{6}$/);
    expect(ratio(t[fg]!, t[bg]!)).toBeGreaterThanOrEqual(min);
  });

  it("checks or exempts every colour token it declares", () => {
    const covered = new Set([...PAIRS.flatMap(([fg, bgs]) => [fg, ...bgs]), ...Object.keys(DECORATIVE)]);
    expect(Object.keys(t).filter((name) => !covered.has(name))).toEqual([]);
  });
});

// Every `selector { declarations }` rule of a sheet, at any nesting depth.
function rules(sheet: string): { selector: string; body: string }[] {
  return [...sheet.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
    selector: m[1]!.trim().replace(/\s+/g, " "),
    body: m[2]!,
  }));
}

const COLOUR_PROPS = /^(color|background(-color)?|border(-[a-z]+)*|outline(-color)?|box-shadow|fill|stroke|text-decoration(-color)?|caret-color|accent-color)$/;
const LITERAL = /#[0-9a-fA-F]{3,8}\b|\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(|\b(white|black|red|green|blue|yellow|orange|gray|grey)\b/;
// Literal colours that are not a theme pair: the dialog scrim darkens whatever is behind it in either theme.
const LITERAL_ALLOWED = new Set([".dialog-backdrop"]);

describe.each(Object.keys(sheets))("%s", (file) => {
  it("takes every colour from a theme token", () => {
    const offenders = rules(sheets[file]!)
      .filter(({ selector }) => !selector.startsWith(":root") && !LITERAL_ALLOWED.has(selector))
      .flatMap(({ selector, body }) =>
        body
          .split(";")
          .map((d) => d.trim())
          .filter((d) => COLOUR_PROPS.test(d.split(":")[0]!.trim()) && LITERAL.test(d.slice(d.indexOf(":") + 1)))
          .map((d) => `${selector} { ${d} }`),
      );
    expect(offenders).toEqual([]);
  });

  it("draws control boundaries and chart axes with --border-strong, not the decorative --border", () => {
    const control = /(^|[\s,])(input|select|textarea)\b|\.button\b|\.segmented label|^\.history-plot$/;
    const offenders = rules(sheets[file]!)
      .filter(({ selector, body }) => control.test(selector) && /border[\w-]*\s*:[^;]*var\(--border\)/.test(body))
      .map(({ selector }) => selector);
    expect(offenders).toEqual([]);
  });
});
