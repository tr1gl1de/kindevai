import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { renderWithI18n } from "./test/render";

function mockFetch(res: Response | Error) {
  const fn = vi.fn(() => (res instanceof Error ? Promise.reject(res) : Promise.resolve(res)));
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("App", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the control plane version when healthy", async () => {
    const fetchMock = mockFetch(Response.json({ status: "ok", version: "1.2.3" }));
    await renderWithI18n(<App />);
    expect(screen.getByRole("heading", { name: "AI Platform" })).toBeInTheDocument();
    expect(await screen.findByText("Control plane online · 1.2.3")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith("/api/health", expect.anything());
  });

  it("reports an unreachable control plane", async () => {
    mockFetch(new Response("down", { status: 503 }));
    await renderWithI18n(<App />);
    expect(await screen.findByText("Control plane unreachable")).toBeInTheDocument();
  });

  it("renders in Russian", async () => {
    mockFetch(Response.json({ status: "ok", version: "1.2.3" }));
    await renderWithI18n(<App />, "ru");
    expect(await screen.findByText("Control plane доступен · 1.2.3")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Язык" })).toBeInTheDocument();
  });
});
