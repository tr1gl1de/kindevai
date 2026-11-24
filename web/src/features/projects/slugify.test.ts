import { describe, expect, it } from "vitest";
import { slugify } from "./api";

describe("slugify", () => {
  it.each([
    ["Web App 2", "web-app-2"],
    ["  Платформа ИИ ", "platforma-ii"],
    ["--A__B--", "a-b"],
    ["x".repeat(50), "x".repeat(40)],
    ["!!!", ""],
  ])("%s → %s", (name, want) => expect(slugify(name)).toBe(want));
});
