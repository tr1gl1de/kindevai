import { getConfig } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import config from "../../vite.config";

// KIN-82: under full `make test` load (Go -race in parallel) the web suite runs ~3x slower than idle.
// Long user scenarios must not depend on vitest's 5 s per-test default, nor findBy/waitFor on the 1 s default.
describe("test timeouts tolerate a loaded machine", () => {
  it("gives every test well above the 5 s vitest default", () => {
    expect(config.test?.testTimeout).toBeGreaterThanOrEqual(30_000);
  });

  it("lets findBy/waitFor wait well above the 1 s Testing Library default", () => {
    expect(getConfig().asyncUtilTimeout).toBeGreaterThanOrEqual(5_000);
    expect(getConfig().asyncUtilTimeout).toBeLessThan(config.test?.testTimeout ?? 0);
  });
});
