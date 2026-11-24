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
  },
});
