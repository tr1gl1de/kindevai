// @vitest-environment node
// KIN-119: the web UI follows the Kindevai Brand System v1.0 — brand palette in the theme tokens (light + dark),
// Nunito Sans / JetBrains Mono served from the app itself (no font CDN), page title and favicon.
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const web = fileURLToPath(new URL("../../", import.meta.url));
const read = (f: string) => readFileSync(`${web}${f}`, "utf8");
const css = read("src/styles.css").replace(/\/\*[\s\S]*?\*\//g, "");

// The value a `<selector> { … }` block of styles.css gives a custom property, its var() fallback when it has one.
function token(selector: string, name: string): string {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) throw new Error(`styles.css has no ${selector} block`);
  const body = css.slice(css.indexOf("{", start) + 1, css.indexOf("}", start));
  const m = new RegExp(`${name}\\s*:\\s*([^;]+);`).exec(body);
  if (!m) throw new Error(`${selector} does not set ${name}`);
  const v = m[1]!.trim();
  return (/(#[0-9a-fA-F]{6})\)?$/.exec(v)?.[1] ?? v).toLowerCase();
}

const DARK = ':root[data-theme="dark"]';

describe("brand palette", () => {
  it.each([
    [":root", "--bg", "#f9f6ec"], // Cream
    [":root", "--surface", "#ffffff"],
    [":root", "--text", "#242420"], // Charcoal
    [":root", "--primary", "#ffb928"], // Honey: the main action
    [":root", "--primary-text", "#242420"], // never white on Honey
    [":root", "--border-strong", "#817f74"],
    [":root", "--danger", "#b42335"],
    [DARK, "--bg", "#191916"],
    [DARK, "--surface", "#242420"],
    [DARK, "--text", "#f9f6ec"],
    [DARK, "--primary", "#ffb928"],
    [DARK, "--primary-text", "#242420"],
    [DARK, "--accent", "#ffb928"],
    [DARK, "--danger", "#ff9aa5"],
  ])("%s sets %s to %s", (selector, name, hex) => {
    expect(token(selector, name)).toBe(hex);
  });

  it("keeps the Telegram Mini App theme in charge of the primary button", () => {
    expect(css).toMatch(/--primary:\s*var\(--tg-theme-button-color,/);
    expect(css).toMatch(/--primary-text:\s*var\(--tg-theme-button-text-color,/);
    // A hover shade of Honey under Telegram's (often white) button text would be unreadable.
    expect(css.match(/--primary-hover:\s*var\(--tg-theme-button-color,/g)).toHaveLength(3);
  });

  it("fills primary buttons and the skip link with the primary tokens", () => {
    for (const selector of [".button.primary", ".skip-link"]) {
      const body = css.slice(css.indexOf(`${selector} {`));
      expect(body.slice(0, body.indexOf("}"))).toMatch(/background:\s*var\(--primary\);[\s\S]*color:\s*var\(--primary-text\);/);
    }
  });
});

describe("typography", () => {
  it("uses Nunito Sans for the interface and JetBrains Mono for code, with system fallbacks", () => {
    expect(css).toMatch(/--font-ui:\s*"Nunito Sans Variable",[^;]*system-ui/);
    expect(css).toMatch(/--font-mono:\s*"JetBrains Mono Variable",[^;]*monospace/);
    expect(css).toMatch(/font-family:\s*var\(--font-ui\)/);
    expect(css).toMatch(/code,\s*pre,\s*kbd,\s*samp\s*\{[^}]*font-family:\s*var\(--font-mono\)/);
  });

  it("serves the fonts from the app bundle, never from a font CDN", () => {
    const main = read("src/main.tsx");
    expect(main).toContain('import "@fontsource-variable/nunito-sans/wght.css";');
    expect(main).toContain('import "@fontsource-variable/jetbrains-mono/wght.css";');
    for (const f of ["index.html", "src/styles.css", "src/main.tsx"]) {
      expect(`${f}: ${read(f)}`).not.toMatch(/fonts\.(googleapis|gstatic)\.com|fonts\.bunny\.net|use\.typekit/);
    }
  });
});

describe("page identity", () => {
  const html = read("index.html");

  it("titles the page Kindevai", () => {
    expect(html).toMatch(/<title>Kindevai<\/title>/);
  });

  it.each([
    ['rel="icon" href="/favicon.svg" type="image/svg+xml"', "public/favicon.svg"],
    ['rel="icon" href="/favicon.ico" sizes="32x32"', "public/favicon.ico"],
  ])("links %s and ships the file", (link, file) => {
    expect(html).toContain(link);
    expect(existsSync(`${web}${file}`)).toBe(true);
  });
});
