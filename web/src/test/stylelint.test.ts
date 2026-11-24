// @vitest-environment node
// KIN-66: a broken stylesheet must fail `npm run lint` (and so `make lint` and CI). Twice a parallel merge dropped a
// closing brace from styles.css; the Vite build still passed while every rule after it was silently swallowed.
import { fileURLToPath } from "node:url";
import stylelint from "stylelint";

const webRoot = fileURLToPath(new URL("../../", import.meta.url));
const configFile = `${webRoot}stylelint.config.mjs`;

async function lintCode(code: string) {
  const { results } = await stylelint.lint({ code, codeFilename: "fixture.css", configFile });
  return results[0]!;
}

describe("stylesheet lint", () => {
  it("fails on an unbalanced brace", async () => {
    const result = await lintCode(".a { color: red;\n\n.b { color: blue; }\n");

    expect(result.errored).toBe(true);
    expect(result.warnings.map((w) => w.rule)).toContain("CssSyntaxError");
  });

  it("fails on a stray closing brace", async () => {
    const result = await lintCode(".a { color: red; }\n}\n.b { color: blue; }\n");

    expect(result.errored).toBe(true);
  });

  it("accepts a well-formed stylesheet", async () => {
    const result = await lintCode(".a { color: red; }\n.b { color: blue; }\n");

    expect(result.errored).toBe(false);
  });

  it("passes for every stylesheet under src", async () => {
    const { errored, results } = await stylelint.lint({ files: `${webRoot}src/**/*.css`, configFile });

    const problems = results.flatMap((r) => r.warnings.map((w) => `${r.source}:${w.line} ${w.text}`));
    expect(problems).toEqual([]);
    expect(errored).toBe(false);
    expect(results.length).toBeGreaterThan(0);
  });
});
