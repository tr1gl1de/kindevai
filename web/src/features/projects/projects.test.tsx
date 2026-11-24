import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { describe, expect, it } from "vitest";
import type { Member, Me, Project } from "../../api/types";
import { apiError, json, owner, server } from "../../test/msw";
import { expectAccessible, renderApp } from "../../test/render";

const web: Project = { id: "p1", slug: "web", name: "Web", myRole: "owner", createdAt: "2026-11-24T10:00:00Z" };

function as(me: Me) {
  server.use(http.get("/api/auth/me", () => json(me)));
}

describe("projects", () => {
  it("shows an empty state that offers to create the first project", async () => {
    as(owner);
    const { container } = await renderApp("/projects");
    expect(await screen.findByText("No projects yet")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New project" })).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("lets the owner create a project with a slug suggested from the name", async () => {
    as(owner);
    const projects: Project[] = [];
    let posted: unknown;
    server.use(
      http.get("/api/projects", () => json(projects)),
      http.post("/api/projects", async ({ request }) => {
        posted = await request.json();
        const p = { ...web, ...(posted as object) } as Project;
        projects.push(p);
        return json(p, 201);
      }),
    );
    await renderApp("/projects");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New project" }));
    const form = screen.getByRole("form", { name: "New project" });
    await user.type(within(form).getByLabelText("Name"), "Web App 2");
    expect(within(form).getByLabelText("Slug")).toHaveValue("web-app-2");
    await user.click(within(form).getByRole("button", { name: "Create project" }));
    expect(await screen.findByRole("link", { name: "Web App 2" })).toBeInTheDocument();
    expect(posted).toEqual({ name: "Web App 2", slug: "web-app-2" });
    expect(screen.getByRole("status")).toHaveTextContent("Project “Web App 2” created.");
  });

  it("shows a taken slug next to the field", async () => {
    as(owner);
    server.use(http.post("/api/projects", () => apiError(409, "slug_taken")));
    await renderApp("/projects");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New project" }));
    await user.type(screen.getByLabelText("Name"), "Web");
    await user.click(screen.getByRole("button", { name: "Create project" }));
    expect(await screen.findByLabelText("Slug")).toHaveAccessibleDescription(/already used/i);
  });

  it("hides owner-only actions from members", async () => {
    as({ id: "u2", login: "bob", role: "user" });
    server.use(http.get("/api/projects", () => json([{ ...web, myRole: "member" }])));
    await renderApp("/projects");
    expect(await screen.findByRole("link", { name: "Web" })).toBeInTheDocument();
    expect(screen.getByText("Member")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New project" })).not.toBeInTheDocument();
  });

  it("manages members on the project page with confirmation", async () => {
    as(owner);
    let members: Member[] = [{ userId: "u2", login: "alice", role: "project_admin" }];
    server.use(
      http.get("/api/projects/p1", () => json(web)),
      http.get("/api/projects/p1/members", () => json(members)),
      http.put("/api/projects/p1/members/:login", async ({ params, request }) => {
        if (params.login === "ghost") return apiError(404, "user_not_found");
        const { role } = (await request.json()) as { role: Member["role"] };
        const m: Member = { userId: "u3", login: String(params.login), role };
        members = [...members.filter((x) => x.login !== m.login), m];
        return json(m);
      }),
      http.delete("/api/projects/p1/members/:login", ({ params }) => {
        members = members.filter((x) => x.login !== params.login);
        return new Response(null, { status: 204 });
      }),
    );
    const { container } = await renderApp("/projects/p1");
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { level: 1, name: "Web" })).toBeInTheDocument();
    expect(await screen.findByRole("cell", { name: "alice" })).toBeInTheDocument();
    await expectAccessible(container);

    const add = screen.getByRole("form", { name: "Add member" });
    await user.type(within(add).getByLabelText("Login"), "ghost");
    await user.click(within(add).getByRole("button", { name: "Add member" }));
    expect(await within(add).findByLabelText("Login")).toHaveAccessibleDescription(/no user with this login/i);

    await user.clear(within(add).getByLabelText("Login"));
    await user.type(within(add).getByLabelText("Login"), "bob");
    await user.selectOptions(within(add).getByLabelText("Role"), "member");
    await user.click(within(add).getByRole("button", { name: "Add member" }));
    expect(await screen.findByRole("cell", { name: "bob" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Remove alice" }));
    const dialog = screen.getByRole("alertdialog", { name: "Remove alice from Web?" });
    await user.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("status")).toHaveTextContent("alice removed from Web.");
    expect(screen.queryByRole("cell", { name: "alice" })).not.toBeInTheDocument();
  });

  it("treats a missing or foreign project as not found", async () => {
    as({ id: "u2", login: "bob", role: "user" });
    server.use(http.get("/api/projects/zzz", () => apiError(404, "not_found")));
    await renderApp("/projects/zzz");
    expect(await screen.findByRole("heading", { level: 1, name: "Project not found" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to projects" })).toBeInTheDocument();
  });
});
