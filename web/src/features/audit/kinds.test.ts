import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { en } from "../../i18n/locales/en";
import { ru } from "../../i18n/locales/ru";
import { kindKey } from "./kinds";

// Every audit kind the Go code records ("Kind... = "<namespace>.<action>"") must have a name in both locales,
// so a task that adds a kind cannot ship without its EN and RU labels.
// A string URL, not new URL(): jsdom replaces the URL global, which fileURLToPath rejects.
const internalDir = join(dirname(fileURLToPath(import.meta.url)), "../../../../internal");

function recordedKinds(): string[] {
  const kinds = new Set<string>();
  const files = readdirSync(internalDir, { recursive: true, encoding: "utf8" })
    .filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"));
  for (const f of files) {
    const src = readFileSync(join(internalDir, f), "utf8");
    for (const m of src.matchAll(/\bKind\w*\s*=\s*"([a-z0-9_]+\.[a-z0-9_]+)"/g)) kinds.add(m[1]!);
  }
  return [...kinds].sort();
}

describe("audit kind labels", () => {
  const kinds = recordedKinds();

  it("finds the kinds recorded by the Go code", () => {
    expect(kinds).toContain("user.created");
    expect(kinds).toContain("runner.enrolled");
    expect(kinds).toContain("integration.youtrack_configured");
  });

  it.each([
    ["en", en.audit.kinds as Record<string, string>],
    ["ru", ru.audit.kinds as Record<string, string>],
  ])("names every recorded kind in %s", (_lang, labels) => {
    const missing = kinds.filter((k) => !labels[kindKey(k)]);
    expect(missing).toEqual([]);
  });
});
