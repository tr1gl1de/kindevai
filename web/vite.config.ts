import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// The build lands in dist/app, which the Go package `web` embeds into the Control Plane binary.
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist/app", emptyOutDir: true },
  server: {
    proxy: { "/api": `http://127.0.0.1:${process.env.KINDEV_PORT ?? "8080"}` },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    // KIN-82: idle, the slowest scenario takes ~2-3 s. Measured next to a Go -race run on a box already
    // oversubscribed by parallel worktrees (load average ~70 on 12 cores) the worst test reached ~15 s. 30 s keeps
    // 2x headroom over that and still fails a hung test quickly. findBy/waitFor get their own 5 s limit
    // (src/test/setup.ts), which stays below this one so a missing element reports "Unable to find…" instead of a
    // bare test timeout.
    testTimeout: 30_000,
  },
});
