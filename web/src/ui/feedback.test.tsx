// KIN-120: Kibi, the Kindevai mascot, appears in benign empty states only — never on errors (Brand System v1.0).
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderWithI18n } from "../test/render";
import { EmptyState, ErrorState } from "./feedback";

const kibi = (root: HTMLElement) => root.querySelectorAll("svg.kibi");

describe("Kibi in feedback states", () => {
  it("greets an empty state with the idle Kibi, hidden from assistive technology", async () => {
    const { container } = await renderWithI18n(<EmptyState title="No projects yet" />);
    const [mascot, ...rest] = kibi(container);
    expect(rest).toEqual([]);
    expect(mascot).toHaveAttribute("aria-hidden", "true");
    expect(mascot).toHaveAttribute("data-mood", "idle");
    expect(screen.getByRole("heading", { name: "No projects yet" })).toBeInTheDocument();
  });

  it("shows the success Kibi when an empty list means all is done", async () => {
    const { container } = await renderWithI18n(<EmptyState title="No active alerts" mood="success" />);
    expect(kibi(container)[0]).toHaveAttribute("data-mood", "success");
  });

  it("never shows Kibi on an error", async () => {
    const { container } = await renderWithI18n(<ErrorState message="Cannot reach the control plane." onRetry={() => {}} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Cannot reach the control plane.");
    expect(kibi(container)).toHaveLength(0);
  });
});
