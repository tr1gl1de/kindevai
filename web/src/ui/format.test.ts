import { describe, expect, it } from "vitest";
import { formatBytes, formatDuration } from "./format";

describe("formatBytes", () => {
  it("picks a unit that never rounds a small size down to zero", () => {
    expect(formatBytes(4 * 1e9, "en")).toBe("4 GB");
    expect(formatBytes(8192, "en")).toBe("8.2 kB");
    expect(formatBytes(512, "en")).toBe("512 byte");
    expect(formatBytes(0, "en")).toBe("0 byte");
  });
});

describe("formatDuration", () => {
  it("shows hours and minutes, or minutes and seconds, in the UI language", () => {
    expect(formatDuration(3_900_000, "en")).toBe("1 hr 5 min");
    expect(formatDuration(65_000, "en")).toBe("1 min 5 sec");
    expect(formatDuration(1_500, "en")).toBe("2 sec");
    expect(formatDuration(0, "en")).toBe("0 sec");
    expect(formatDuration(3_600_000, "ru")).toBe("1 ч");
  });
});
