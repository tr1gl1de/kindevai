import { describe, expect, it } from "vitest";
import { formatBytes } from "./format";

describe("formatBytes", () => {
  it("picks a unit that never rounds a small size down to zero", () => {
    expect(formatBytes(4 * 1e9, "en")).toBe("4 GB");
    expect(formatBytes(8192, "en")).toBe("8.2 kB");
    expect(formatBytes(512, "en")).toBe("512 byte");
    expect(formatBytes(0, "en")).toBe("0 byte");
  });
});
