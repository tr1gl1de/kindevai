// @vitest-environment node
// KIN-120: Brand System v1.0 layout, type scale and motion (components.md, brandbook §04). jsdom has no layout, so
// the stylesheets are the contract here; Playwright checks the rendered result at 360/768/1440 px.
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

const src = fileURLToPath(new URL("..", import.meta.url));
const strip = (s: string) => s.replace(/\/\*[\s\S]*?\*\//g, "");
const base = strip(readFileSync(`${src}/styles.css`, "utf8"));
const sheets: Record<string, string> = {
  "styles.css": base,
  ...Object.fromEntries(
    readdirSync(`${src}/features`, { withFileTypes: true })
      .filter((d) => d.isDirectory())
      .flatMap((d) =>
        readdirSync(`${src}/features/${d.name}`)
          .filter((f) => f.endsWith(".css"))
          .map((f) => [`features/${d.name}/${f}`, strip(readFileSync(`${src}/features/${d.name}/${f}`, "utf8"))]),
      ),
  ),
};

// Every `selector { declarations }` rule with the @media condition it sits in ("" at top level). Nested rules are
// rules of their own; their parent keeps only its own declarations.
type Rule = { media: string; selector: string; decls: Record<string, string> };
function rules(css: string): Rule[] {
  const out: Rule[] = [];
  const stack: { at: boolean; head: string; body: string }[] = [];
  let head = "";
  for (const ch of css) {
    if (ch === "{") {
      stack.push({ at: head.trim().startsWith("@"), head: head.trim().replace(/\s+/g, " "), body: "" });
      head = "";
    } else if (ch === ";") {
      if (stack.length) stack[stack.length - 1]!.body += `${head};`;
      head = "";
    } else if (ch === "}") {
      const top = stack.pop();
      if (top && !top.at) {
        const decls = Object.fromEntries(
          `${top.body}${head}`
            .split(";")
            .filter((d) => d.includes(":"))
            .map((d) => [d.slice(0, d.indexOf(":")).trim(), d.slice(d.indexOf(":") + 1).trim()]),
        );
        out.push({ media: stack.filter((s) => s.at).map((s) => s.head).join(" "), selector: top.head, decls });
      }
      head = "";
    } else {
      head += ch;
    }
  }
  return out;
}

const all = Object.entries(sheets).flatMap(([file, css]) => rules(css).map((r) => ({ file, ...r })));
const find = (selector: string, media = "") =>
  all.filter((r) => r.selector.split(",").map((s) => s.trim()).includes(selector) && r.media === media);
const decl = (selector: string, prop: string, media = "") =>
  find(selector, media).map((r) => r.decls[prop]).filter((v) => v !== undefined).at(-1);

const DESKTOP = "@media (min-width: 1024px)";

describe("layout", () => {
  it("puts the navigation in a 240–264 px sidebar from 1024 px, under a 64 px header", () => {
    expect(decl(".shell", "grid-template-columns", DESKTOP)).toMatch(/^var\(--sidebar\) minmax\(0, 1fr\)$/);
    const sidebar = parseInt(decl(":root", "--sidebar") ?? "", 10);
    expect(sidebar).toBeGreaterThanOrEqual(240);
    expect(sidebar).toBeLessThanOrEqual(264);
    expect(decl(":root", "--header")).toBe("64px");
    expect(decl(".topbar", "min-height")).toBe("var(--header)");
    expect(decl(".sidebar", "display", DESKTOP)).toBe("flex");
    expect(decl(".menu-button", "display", DESKTOP)).toBe("none");
    expect(decl(".sidebar", "align-self", DESKTOP)).toBe("start");
    // Sticky header: focus and anchors land below it (WCAG 2.4.11).
    expect(decl("html", "scroll-padding-top", DESKTOP)).toBe("var(--header)");
  });

  it("hides the navigation drawer below 1024 px until the Menu button opens it", () => {
    expect(decl(".sidebar", "display", "@media (max-width: 1023.98px)")).toBe("none");
    expect(decl('.shell[data-nav="open"] .sidebar', "display", "@media (max-width: 1023.98px)")).toBe("flex");
  });

  it("keeps the content within 1600 px and long text within 72ch", () => {
    expect(decl(".content", "max-width")).toBe("1600px");
    expect(decl(".content p", "max-width")).toBe("72ch");
  });
});

// Brandbook §04: h1 32/40, h2 24/32, h3 20/28, body 16/24, compact 14/20, caption 12/18, code 13/21.
const SCALE: Record<string, [string, string]> = {
  h1: ["32px", "40px"],
  h2: ["24px", "32px"],
  h3: ["20px", "28px"],
  body: ["16px", "24px"],
  compact: ["14px", "20px"],
  caption: ["12px", "18px"],
  code: ["13px", "21px"],
};

describe("type scale", () => {
  it.each(Object.entries(SCALE))("declares %s as %s", (step, [size, leading]) => {
    expect(decl(":root", `--size-${step}`)).toBe(size);
    expect(decl(":root", `--leading-${step}`)).toBe(leading);
  });

  it.each([
    ["h1", "h1"],
    [".page-heading", "h1"],
    ["h2", "h2"],
    ["h3", "h3"],
    ["body", "body"],
    ["code", "code"],
  ])("sets %s in the %s step", (selector, step) => {
    expect(decl(selector, "font-size")).toBe(`var(--size-${step})`);
    expect(decl(selector, "line-height")).toBe(`var(--leading-${step})`);
  });

  it("takes every font size and its line height from the scale", () => {
    const offenders = all
      .filter((r) => !r.selector.startsWith(":root") && r.decls["font-size"] !== undefined)
      .filter((r) => {
        const step = /^var\(--size-(\w+)\)$/.exec(r.decls["font-size"]!)?.[1];
        return !step || !(step in SCALE) || r.decls["line-height"] !== `var(--leading-${step})`;
      })
      .map((r) => `${r.file} ${r.selector} { font-size: ${r.decls["font-size"]}; line-height: ${r.decls["line-height"]} }`);
    expect(offenders).toEqual([]);
  });

  it("aligns digits in table cells", () => {
    expect(decl("td", "font-variant-numeric")).toBe("tabular-nums");
  });
});

describe("motion", () => {
  it("declares the 120/180/240 ms durations", () => {
    expect(decl(":root", "--motion-fast")).toBe("120ms");
    expect(decl(":root", "--motion-base")).toBe("180ms");
    expect(decl(":root", "--motion-slow")).toBe("240ms");
  });

  it("times every transition with a motion token", () => {
    const transitions = all.filter((r) => r.decls.transition !== undefined && !r.media.includes("prefers-reduced-motion"));
    expect(transitions.length).toBeGreaterThan(0);
    const offenders = transitions
      .filter((r) => /\d(m?s)\b/.test(r.decls.transition!) || !/var\(--motion-(fast|base|slow)\)/.test(r.decls.transition!))
      .map((r) => `${r.file} ${r.selector} { transition: ${r.decls.transition} }`);
    expect(offenders).toEqual([]);
  });

  it("turns motion off when the user prefers reduced motion", () => {
    const reduced = all.filter((r) => r.media === "@media (prefers-reduced-motion: reduce)" && r.selector === "*");
    expect(reduced.map((r) => r.decls.transition)).toContain("none !important");
    expect(reduced.map((r) => r.decls.animation)).toContain("none !important");
  });
});
