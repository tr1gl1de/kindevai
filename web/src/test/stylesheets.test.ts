// @vitest-environment node
// KIN-68: styles.css holds the base (tokens, layout, shared components); each feature's rules live next to it in
// features/<context>/<context>.css, imported by that feature's components. The bundle keeps import order, so the
// base must be imported before anything that pulls in a feature stylesheet: feature rules then follow the base
// rules they refine, as they did in the single stylesheet.
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const src = fileURLToPath(new URL("../", import.meta.url));

function files(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? files(join(dir, e.name)) : [join(dir, e.name)],
  );
}

const all = files(src).map((f) => relative(src, f).replaceAll("\\", "/"));
const sources = all.filter((f) => /\.tsx?$/.test(f) && !/\.test\.tsx?$/.test(f));
// The classes of each outermost selector of a stylesheet (nested rules count for their parent; @media rules are
// looked into).
function selectorClasses(css: string): { selector: string; classes: string[] }[] {
  const out: { selector: string; classes: string[] }[] = [];
  const stack: boolean[] = []; // per open block: is it inside a style rule
  let head = "";
  for (const ch of css.replace(/\/\*[\s\S]*?\*\//g, "")) {
    if (ch === "{") {
      const inRule = stack.includes(true);
      const atRule = head.trim().startsWith("@");
      if (!inRule && !atRule) {
        for (const sel of head.split(",")) {
          const classes = [...sel.matchAll(/\.([A-Za-z][\w-]*)/g)].map((m) => m[1]!);
          if (classes.length > 0) out.push({ selector: sel.trim(), classes });
        }
      }
      stack.push(inRule || !atRule);
      head = "";
    } else if (ch === "}") {
      stack.pop();
      head = "";
    } else if (ch === ";") {
      head = "";
    } else {
      head += ch;
    }
  }
  return out;
}

const imports = (file: string) =>
  [...readFileSync(join(src, file), "utf8").matchAll(/^import\s+(?:[^"']*from\s+)?["']([^"']+)["']/gm)].map((m) => m[1]!);

describe("stylesheets", () => {
  it("imports the base stylesheet before anything else", () => {
    expect(imports("main.tsx")[0]).toBe("./styles.css");
  });

  it("keeps feature stylesheets inside their feature and imports each one there", () => {
    const sheets = all.filter((f) => f.endsWith(".css") && f !== "styles.css");
    expect(sheets.length).toBeGreaterThan(0);
    for (const sheet of sheets) {
      const [, feature, name] = sheet.match(/^features\/([^/]+)\/([^/]+)$/) ?? [];
      expect(feature, `${sheet} is not under features/<context>/`).toBeDefined();
      const importers = sources.filter((f) => imports(f).includes(`./${name}`) && f.startsWith(`features/${feature}/`));
      expect(importers, `${sheet} is not imported by its feature`).not.toEqual([]);
      const outside = sources.filter((f) => !f.startsWith(`features/${feature}/`) && imports(f).some((i) => i.endsWith(`/${name}`)));
      expect(outside, `${sheet} is imported from outside its feature`).toEqual([]);
    }
  });

  it("puts a feature class in every selector of a feature stylesheet", () => {
    const sheets = all.filter((f) => /^features\/[^/]+\/[^/]+\.css$/.test(f));
    for (const sheet of sheets) {
      const feature = sheet.split("/")[1]!;
      const others = sources.filter((f) => !f.startsWith(`features/${feature}/`));
      const outside = (cls: string) => {
        const used = new RegExp(`className=[^>]*(?<![\\w-])${cls}(?![\\w-])`);
        return others.some((f) => used.test(readFileSync(join(src, f), "utf8")));
      };
      for (const { selector, classes } of selectorClasses(readFileSync(join(src, sheet), "utf8"))) {
        expect(classes.some((c) => !outside(c)), `${selector} in ${sheet} styles only classes used outside ${feature}; move it to styles.css`).toBe(true);
      }
    }
  });

  // KIN-109: before KIN-68 these accents sat in sheets bundled ahead of the base, so the base .badge/.card border
  // shorthand always won. Feature sheets now follow the base, so the accents render; keep them declared.
  it.each([
    ["features/tasks/tasks.css", ".time-incomplete", "border-color: var(--warn-border)"],
    ["features/tasks/tasks.css", ".ws-note", "border-color: var(--warn-border)"],
    ["features/tasks/tasks.css", ".tests-not_configured", "border-color: var(--warn-border)"],
    ["features/tasks/tasks.css", ".ws-status-D", "border-color: var(--danger)"],
    ["features/tasks/tasks.css", ".tests-failed", "border-color: var(--danger)"],
    ["features/tasks/tasks.css", ".tests-missing", "border-color: var(--danger)"],
    ["features/tasks/tasks.css", ".verdict-changes_requested", "border-color: var(--danger)"],
    ["features/planDiscussion/planDiscussion.css", ".discussion-status.status-FAILED", "border-color: var(--danger)"],
    ["features/planDiscussion/planDiscussion.css", ".discussion-status.status-PENDING", "border-color: var(--warn-border)"],
    ["features/planDiscussion/planDiscussion.css", ".discussion-status.status-DELIVERED", "border-color: var(--warn-border)"],
    ["features/planDiscussion/planDiscussion.css", ".discussion-revision-current", "border: 2px solid var(--accent)"],
    ["features/planDiscussion/planDiscussion.css", ".discussion-status", "display: inline-flex"],
  ])("%s gives %s the accent %s", (sheet, selector, declaration) => {
    expect(declarations(readFileSync(join(src, sheet), "utf8"), selector)).toContain(declaration);
  });
});

// The declarations of the top-level rules whose selector list contains exactly `selector`.
function declarations(css: string, selector: string): string[] {
  const out: string[] = [];
  for (const m of css.replace(/\/\*[\s\S]*?\*\//g, "").matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (m[1]!.split(",").some((s) => s.trim() === selector)) {
      out.push(...m[2]!.split(";").map((d) => d.trim().replace(/\s+/g, " ")).filter(Boolean));
    }
  }
  return out;
}
