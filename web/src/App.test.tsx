import { screen } from "@testing-library/react";
import { HttpResponse, http } from "msw";
import { describe, expect, it } from "vitest";
import { App } from "./App";
import { server } from "./test/msw";
import { renderWithI18n } from "./test/render";

const healthy = http.get("/api/health", () => HttpResponse.json({ status: "ok", version: "1.2.3" }));

describe("App", () => {
  it("shows the control plane version when healthy", async () => {
    server.use(healthy);
    await renderWithI18n(<App />);
    expect(screen.getByRole("heading", { name: "AI Platform" })).toBeInTheDocument();
    expect(await screen.findByText("Control plane online · 1.2.3")).toBeInTheDocument();
  });

  it("reports an unreachable control plane", async () => {
    server.use(http.get("/api/health", () => new HttpResponse("down", { status: 503 })));
    await renderWithI18n(<App />);
    expect(await screen.findByText("Control plane unreachable")).toBeInTheDocument();
  });

  it("renders in Russian", async () => {
    server.use(healthy);
    await renderWithI18n(<App />, "ru");
    expect(await screen.findByText("Control plane доступен · 1.2.3")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Язык" })).toBeInTheDocument();
  });
});
