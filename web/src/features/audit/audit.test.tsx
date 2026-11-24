import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { AuditEvent, AuditPage, Me, Project } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, renderApp } from "../../test/render";

const alpha: Project = { id: "p1", slug: "alpha", name: "Alpha", createdAt: "2026-11-24T10:00:00Z", myRole: "owner" };

const ev = (id: number, kind: string, extra: Partial<AuditEvent> = {}): AuditEvent => ({
  id,
  at: "2026-11-24T10:00:00Z",
  actor: { type: "user", id: "u1", label: "owner" },
  kind,
  ...extra,
});

describe("audit log (owner)", () => {
  it("lists events, filters them and loads more", async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/projects", () => json([alpha])),
      http.get("/api/audit/kinds", () => json(["invitation.accepted", "invitation.created", "user.created", "runner.enrolled"])),
      http.get("/api/audit", ({ request }) => {
        const q = new URL(request.url).searchParams;
        queries.push(q);
        if (q.get("project") === "p1") {
          return json<AuditPage>({ events: [ev(2, "invitation.created", { projectId: "p1", subject: "invitation:i1" })], next: null });
        }
        if (q.get("before") === "2") {
          return json<AuditPage>({ events: [ev(1, "runner.enrolled", { actor: { type: "system", id: "", label: "system" } })], next: null });
        }
        return json<AuditPage>({
          events: [
            ev(3, "invitation.accepted", {
              projectId: "p1",
              actor: { type: "user", id: "u2", label: "carol" },
              details: { role: "member", token: "[redacted]" },
            }),
            ev(2, "user.created", { subject: "user:u2", details: { login: "carol" } }),
          ],
          next: "2",
        });
      }),
    );
    const { container } = await renderApp("/audit");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Audit log" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Audit log" })).toHaveAttribute("href", "/audit");
    const table = await screen.findByRole("table", { name: "Audit log" });
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("Invitation accepted");
    expect(rows[1]).toHaveTextContent("carol");
    expect(rows[1]).toHaveTextContent("Alpha");
    expect(rows[1]).toHaveTextContent("[redacted]");
    expect(rows[2]).toHaveTextContent("User created");
    expect(rows[2]).toHaveTextContent("Platform");
    expect(screen.getByText("2 events shown.")).toBeInTheDocument();
    await expectAccessible(container);

    await user.click(screen.getByRole("button", { name: "Load more events" }));
    expect(await screen.findByText("3 events shown.")).toBeInTheDocument();
    expect(within(table).getAllByRole("row")[3]).toHaveTextContent("System");
    expect(within(table).getAllByRole("row")[3]).toHaveTextContent("runner.enrolled");
    expect(screen.queryByRole("button", { name: "Load more events" })).not.toBeInTheDocument();

    const form = screen.getByRole("form", { name: "Filter events" });
    await user.selectOptions(within(form).getByLabelText("Project"), "Alpha");
    await user.selectOptions(within(form).getByLabelText("Event"), "Invitation created");
    await user.type(within(form).getByLabelText("Actor"), "owner");
    await user.click(within(form).getByRole("button", { name: "Apply filters" }));
    expect(await screen.findByText("1 event shown.")).toBeInTheDocument();
    const last = queries.at(-1)!;
    expect(last.get("project")).toBe("p1");
    expect(last.get("kind")).toBe("invitation.created");
    expect(last.get("actor")).toBe("owner");

    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(await screen.findByText("3 events shown.")).toBeInTheDocument(); // unfiltered pages come back from the cache
    expect(within(form).getByLabelText("Project")).toHaveValue("");
    expect(screen.queryByRole("button", { name: "Clear filters" })).not.toBeInTheDocument();
  });

  it("sends the date range as local day bounds and rejects an inverted range", async () => {
    const queries: URLSearchParams[] = [];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/audit/kinds", () => json([])),
      http.get("/api/audit", ({ request }) => {
        queries.push(new URL(request.url).searchParams);
        return json<AuditPage>({ events: [], next: null });
      }),
    );
    await renderApp("/audit");
    const user = userEvent.setup();
    expect(await screen.findByText("Nothing has been recorded yet.")).toBeInTheDocument();
    const form = screen.getByRole("form", { name: "Filter events" });
    await user.type(within(form).getByLabelText("From"), "2026-11-24");
    await user.type(within(form).getByLabelText("To"), "2026-11-20");
    await user.click(within(form).getByRole("button", { name: "Apply filters" }));
    expect(within(form).getByLabelText("To")).toHaveAccessibleDescription(/not be before/i);
    expect(within(form).getByLabelText("To")).toHaveFocus();
    await user.clear(within(form).getByLabelText("To"));
    await user.type(within(form).getByLabelText("To"), "2026-11-24");
    await user.click(within(form).getByRole("button", { name: "Apply filters" }));
    expect(await screen.findByText("No events match these filters.")).toBeInTheDocument();
    const q = queries.at(-1)!;
    expect(q.get("from")).toBe(new Date(2026, 10, 24).toISOString());
    expect(q.get("to")).toBe(new Date(2026, 10, 25).toISOString());
  });

  it("shows an error with a retry", async () => {
    let fail = true;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/audit/kinds", () => json([])),
      http.get("/api/audit", () => (fail ? apiError(500, "internal") : json<AuditPage>({ events: [ev(1, "user.created")], next: null }))),
    );
    await renderApp("/audit");
    const user = userEvent.setup();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("1 event shown.")).toBeInTheDocument();
  });

  it("is translated into Russian", async () => {
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/audit/kinds", () => json(["user.disabled"])),
      http.get("/api/audit", () => json<AuditPage>({ events: [ev(1, "user.disabled")], next: null })),
    );
    const { container } = await renderApp("/audit", "ru");
    expect(await screen.findByRole("heading", { level: 1, name: "Журнал аудита" })).toBeInTheDocument();
    expect(await screen.findByText("Пользователь отключён")).toBeInTheDocument();
    expect(screen.getByText("Показано 1 событие.")).toBeInTheDocument();
    await expectAccessible(container);
  });
});

describe("audit log (non-owner)", () => {
  it("has no audit link or page", async () => {
    const member: Me = { id: "u2", login: "carol", role: "user", mustChangePassword: false };
    server.use(http.get("/api/auth/me", () => json(member)));
    await renderApp("/audit");
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Audit log" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { level: 1, name: "Audit log" })).not.toBeInTheDocument();
  });
});
