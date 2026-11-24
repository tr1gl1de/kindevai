import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import type { NetworkAssignment, NetworkProfile, NetworkProfileRequest, NetworkTest, Runner } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";
import { parseAllow } from "./NetworkPage";

const profile: NetworkProfile = {
  id: "p1",
  name: "Corp proxy",
  proxy: { scheme: "socks5", host: "proxy.example", port: 1080, username: "alice", url: "socks5://alice@proxy.example:1080" },
  passwordSet: true,
  enforcement: "strict",
  dns: "proxy",
  failClosed: true,
  expectedIpv4: "203.0.113.7",
  expectedIpv6: "",
  allow: [],
  diagnosticUrl: "",
  assigned: [{ level: "system", target: "" }],
  createdAt: "2026-11-25T10:00:00Z",
  updatedAt: "2026-11-25T10:00:00Z",
};

const runner = { id: "r1", status: "approved", hostname: "wsl-box", projects: [] } as unknown as Runner;

const test1: NetworkTest = {
  id: "t1",
  runnerId: "r1",
  profileId: "",
  status: "failed",
  code: "egress_ip_mismatch",
  detail: "",
  expectedIpv4: "203.0.113.7",
  expectedIpv6: "",
  observedIpv4: "198.51.100.1",
  observedIpv6: "",
  checks: [{ name: "github", host: "github.com", ok: true, code: "", latencyMs: 80 }],
  durationMs: 900,
  requestedAt: "2026-11-25T10:00:00Z",
  completedAt: "2026-11-25T10:00:02Z",
};

function setup(opts: { profiles?: NetworkProfile[]; assignments?: NetworkAssignment[]; tests?: NetworkTest[] } = {}) {
  server.use(
    http.get("/api/auth/me", () => json(owner)),
    http.get("/api/network/profiles", () => json(opts.profiles ?? [profile])),
    http.get("/api/network/assignments", () => json(opts.assignments ?? [{ level: "system", target: "", profileId: "p1" }])),
    http.get("/api/network/tests", () => json(opts.tests ?? [test1])),
    http.get("/api/runners", () => json([runner])),
    http.get("/api/projects", () => json([])),
  );
}

describe("network page", () => {
  it("lists profiles without passwords, shows the strict lock and recent tests", async () => {
    setup();
    const { container } = await renderApp("/network");
    const card = (await screen.findByRole("heading", { name: "Corp proxy" })).closest("li")!;
    expect(within(card).getByText("socks5://alice@proxy.example:1080")).toBeInTheDocument();
    expect(within(card).getByText("Stored (hidden)")).toBeInTheDocument();
    expect(within(card).getByText(/Strict egress, fail-closed/)).toBeInTheDocument();
    expect(await screen.findByText(/Strict egress is set for the whole installation/)).toBeInTheDocument();
    expect(await screen.findByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("the egress IP is not the expected one")).toBeInTheDocument();
    expect(screen.getByText("198.51.100.1")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Whole installation" })).toHaveValue("p1");
    expect(screen.getByRole("combobox", { name: "CLI claude" })).toHaveValue("");
    await expectAccessible(container);
  });

  it("creates a profile from a proxy URL and maps server validation to the field", async () => {
    setup({ profiles: [], assignments: [], tests: [] });
    let body: NetworkProfileRequest | undefined;
    let calls = 0;
    server.use(
      http.post("/api/network/profiles", async ({ request }) => {
        calls++;
        body = (await request.json()) as NetworkProfileRequest;
        if (calls === 1) return HttpResponse.json({ error: "validation", message: "allow_cidr_too_wide", field: "allow" }, { status: 422 });
        return json({ ...profile, name: body.name }, 201);
      }),
    );
    const user = userEvent.setup();
    const { container } = await renderApp("/network");
    expect(await screen.findByText("No network profiles yet")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "New profile" }));
    await user.click(screen.getByRole("button", { name: "Save profile" }));
    expect(screen.getByLabelText("Name")).toHaveFocus();
    await user.type(screen.getByLabelText("Name"), "Corp");
    await user.type(screen.getByLabelText("Proxy URL"), "socks5://alice:pw@proxy.example:1080");
    await user.type(screen.getByLabelText("Proxy password"), "s3cret");
    await user.selectOptions(screen.getByLabelText("Enforcement"), "strict");
    await user.type(screen.getByLabelText("Allowed destinations"), "0.0.0.0/0 443");
    await user.click(screen.getByRole("button", { name: "Save profile" }));
    expect(await screen.findByText(/The range is too wide/)).toBeInTheDocument();
    expect(screen.getByLabelText("Allowed destinations")).toHaveFocus();
    expect(body).toMatchObject({ name: "Corp", password: "s3cret", enforcement: "strict", allow: [{ cidr: "0.0.0.0/0", ports: [443] }] });
    await expectAccessible(container);
    await user.clear(screen.getByLabelText("Allowed destinations"));
    await user.click(screen.getByRole("button", { name: "Save profile" }));
    await expectStatus("Profile Corp saved.");
  });

  it("assigns a profile and runs Test Network on a runner", async () => {
    setup({ assignments: [], tests: [] });
    let assigned: NetworkAssignment | undefined;
    let requested: unknown;
    server.use(
      http.put("/api/network/assignments", async ({ request }) => {
        assigned = (await request.json()) as NetworkAssignment;
        return new HttpResponse(null, { status: 204 });
      }),
      http.post("/api/network/tests", async ({ request }) => {
        requested = await request.json();
        return json({ ...test1, status: "pending", code: "" }, 202);
      }),
    );
    const user = userEvent.setup();
    await renderApp("/network");
    await user.selectOptions(await screen.findByRole("combobox", { name: "Runner wsl-box" }), "p1");
    await waitFor(() => expect(assigned).toEqual({ level: "runner", target: "r1", profileId: "p1" }));
    await expectStatus(/Assignment saved/);
    await user.click(screen.getByRole("button", { name: "Run Test Network" }));
    await waitFor(() => expect(requested).toEqual({ runnerId: "r1", profileId: "" }));
  });

  it("asks before deleting and explains a profile in use, in Russian", async () => {
    setup();
    server.use(http.delete("/api/network/profiles/p1", () => apiError(409, "in_use", "assigned")));
    const user = userEvent.setup();
    await renderApp("/network", "ru");
    await user.click(await screen.findByRole("button", { name: "Удалить профиль Corp proxy" }));
    const dialog = screen.getByRole("alertdialog", { name: "Удалить профиль Corp proxy?" });
    await user.click(within(dialog).getByRole("button", { name: "Удалить" }));
    expect(await screen.findByText("Профиль назначен. Сначала снимите назначения.")).toBeInTheDocument();
  });
});

describe("parseAllow", () => {
  it("parses hosts and CIDRs with ports and rejects malformed lines", () => {
    expect(parseAllow("*.github.com 443,22\n\n203.0.113.0/24 8443")).toEqual([
      { host: "*.github.com", ports: [443, 22] },
      { cidr: "203.0.113.0/24", ports: [8443] },
    ]);
    expect(parseAllow("api.example.com")).toBeUndefined();
    expect(parseAllow("api.example.com 0")).toBeUndefined();
    expect(parseAllow("a b c")).toBeUndefined();
  });
});
