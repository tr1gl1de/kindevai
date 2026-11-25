import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Alert, AlertAnalysisPreview } from "../../api/types";
import { json, owner, server } from "../../test/msw";
import { expectAccessible, expectStatus, renderApp } from "../../test/render";

const ci: Alert = {
  id: "C1",
  projectId: "p1",
  projectName: "Alpha",
  kind: "ci_failed",
  severity: "warning",
  subject: "acme/app dev",
  details: { conclusion: "failure" },
  state: "OPEN",
  occurrences: 1,
  firstSeenAt: "2026-11-25T10:00:00Z",
  lastSeenAt: "2026-11-25T10:20:00Z",
  ackedAt: null,
  closedAt: null,
  canManage: true,
  canSuggestAnalysis: true,
  analysisTask: null,
};

const preview: AlertAnalysisPreview = {
  summary: "Analyze alert: CI failed on acme/app dev (failure).",
  description: "Analysis task suggested from a kindevai alert.\n\n- Project: Alpha\n\nkindevai-alert-C1",
  configured: true,
  targets: [{ projectId: "p1", projectName: "Alpha", youtrackProject: "SHOP" }],
  task: null,
};

describe("analysis task from an alert (KIN-61)", () => {
  it("previews the draft and creates nothing until confirmed", async () => {
    let posted: unknown;
    let langs: string[] = [];
    let alerts: Alert[] = [ci];
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/alerts", () => json(alerts)),
      http.get("/api/alerts/C1/analysis-task", ({ request }) => {
        langs = [...langs, new URL(request.url).searchParams.get("lang") ?? ""];
        return json(preview);
      }),
      http.post("/api/alerts/C1/analysis-task", async ({ request }) => {
        posted = await request.json();
        alerts = [
          {
            ...ci,
            analysisTask: {
              projectId: "p1",
              youtrackProject: "SHOP",
              requestedAt: ci.lastSeenAt,
              createdAt: null,
            },
          },
        ];
        return json(alerts[0]);
      }),
    );
    await renderApp("/alerts");
    const user = userEvent.setup();
    const row = (await screen.findByText("CI failed on acme/app dev (failure)")).closest("tr")!;
    await user.click(
      within(row).getByRole("button", {
        name: "Suggest an analysis task in YouTrack: CI failed on acme/app dev (failure)",
      }),
    );
    const dialog = await screen.findByRole("alertdialog", {
      name: "Create an analysis task in YouTrack?",
    });
    expect(await within(dialog).findByText(preview.summary)).toBeInTheDocument();
    expect(within(dialog).getByText(/kindevai-alert-C1/)).toBeInTheDocument();
    expect(within(dialog).getByText("Alpha → SHOP")).toBeInTheDocument();
    await expectAccessible(dialog);
    expect(langs).toEqual(["en"]);

    // Cancel creates nothing.
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(posted).toBeUndefined();

    await user.click(within(row).getByRole("button", { name: /^Suggest an analysis task/ }));
    const again = await screen.findByRole("alertdialog");
    await user.click(await within(again).findByRole("button", { name: "Create task" }));
    await expectStatus("Analysis task requested. It appears in YouTrack in a moment.");
    expect(posted).toEqual({ language: "en", projectId: "p1" });
    await waitFor(() => expect(within(row).getByText("Analysis task requested (SHOP)")).toBeInTheDocument());
    expect(within(row).queryByRole("button", { name: /^Suggest an analysis task/ })).not.toBeInTheDocument();
  });

  it("links the created issue and explains a missing YouTrack connection in Russian", async () => {
    const created: Alert = {
      ...ci,
      id: "C2",
      analysisTask: {
        projectId: "p1",
        youtrackProject: "SHOP",
        requestedAt: ci.lastSeenAt,
        createdAt: ci.lastSeenAt,
        issueId: "SHOP-12",
        issueUrl: "https://yt.example/issue/SHOP-12",
      },
    };
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/alerts", () => json([ci, created])),
      http.get("/api/alerts/C1/analysis-task", () => json({ ...preview, configured: false, targets: [] })),
    );
    await renderApp("/alerts", "ru");
    const link = await screen.findByRole("link", {
      name: "Задача анализа SHOP-12",
    });
    expect(link).toHaveAttribute("href", "https://yt.example/issue/SHOP-12");
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /^Предложить задачу анализа/ }));
    const dialog = await screen.findByRole("alertdialog", {
      name: "Создать задачу анализа в YouTrack?",
    });
    expect(
      await within(dialog).findByText("YouTrack не подключён. Owner подключает его на странице «Интеграции»."),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Создать задачу" })).toBeDisabled();
  });

  it("lets the Owner pick the YouTrack project of a platform alert", async () => {
    const platform: Alert = {
      ...ci,
      id: "R1",
      projectId: undefined,
      projectName: undefined,
      kind: "runner_unreachable",
      subject: "r1",
    };
    let posted: unknown;
    server.use(
      http.get("/api/auth/me", () => json(owner)),
      http.get("/api/alerts", () => json([platform])),
      http.get("/api/alerts/R1/analysis-task", () =>
        json({
          ...preview,
          targets: [
            { projectId: "p1", projectName: "Alpha", youtrackProject: "SHOP" },
            { projectId: "p2", projectName: "Beta", youtrackProject: "OPS" },
          ],
        }),
      ),
      http.post("/api/alerts/R1/analysis-task", async ({ request }) => {
        posted = await request.json();
        return json(platform);
      }),
    );
    await renderApp("/alerts");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /^Suggest an analysis task/ }));
    const dialog = await screen.findByRole("alertdialog");
    await user.selectOptions(await within(dialog).findByRole("combobox", { name: "YouTrack project" }), "p2");
    await user.click(within(dialog).getByRole("button", { name: "Create task" }));
    await waitFor(() => expect(posted).toEqual({ language: "en", projectId: "p2" }));
  });
});
