import "@testing-library/jest-dom/vitest";
import { cleanup, configure } from "@testing-library/react";
import { afterAll, afterEach, beforeAll } from "vitest";
import { server } from "./msw";

// KIN-82: findBy*/waitFor give up after 1 s by default; an MSW round trip plus a React render can exceed that when
// the machine is loaded (make test runs Go -race in parallel). 5 s also covers one background poll (1.5 s) under
// load and stays below the per-test timeout in vite.config.ts. Passing checks return as soon as they succeed.
configure({ asyncUtilTimeout: 5_000 });

beforeAll(() => server.listen({ onUnhandledFrame: "error" }));
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());
