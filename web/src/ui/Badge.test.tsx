// KIN-120: a state badge carries its tone three ways — text, an icon and a colour — never colour alone.
import { screen } from "@testing-library/react";
import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { renderWithI18n } from "../test/render";
import { Badge } from "./Badge";
import { AttemptStateBadge, StepStateBadge, TaskStateBadge } from "../features/tasks/badges";
import { AvailabilityBadge } from "../features/runners/AvailabilityBadge";
import { StatusBadge } from "../features/runners/StatusBadge";
import { StateBadge } from "../features/approvals/badges";

const icon = (badge: HTMLElement) => badge.querySelector("svg.badge-icon");

describe("Badge", () => {
  it.each(["info", "success", "warning", "danger"] as const)("shows a decorative icon with the %s tone", async (tone) => {
    await renderWithI18n(<Badge tone={tone}>State</Badge>);
    const badge = screen.getByText("State");
    expect(badge).toHaveClass("badge", `tone-${tone}`);
    expect(icon(badge)).toHaveAttribute("aria-hidden", "true");
    expect(icon(badge)).toHaveAttribute("data-tone", tone);
  });

  it("shows text only for a neutral state", async () => {
    await renderWithI18n(<Badge>Queued</Badge>);
    const badge = screen.getByText("Queued");
    expect(badge).toHaveClass("badge", "tone-neutral");
    expect(icon(badge)).toBeNull();
  });
});

// The visual mapping of Brand System components.md: queued → neutral, planning/running → info, awaiting approval →
// warning, succeeded → success, failed → danger, cancelled → neutral, offline → neutral/warning with its own label.
describe("state badges", () => {
  it.each<[string, () => ReactElement, string]>([
    ["TaskStateBadge CREATED", () => <TaskStateBadge state="CREATED" />, "neutral"],
    ["TaskStateBadge ANALYZING", () => <TaskStateBadge state="ANALYZING" />, "info"],
    ["TaskStateBadge RUNNING", () => <TaskStateBadge state="RUNNING" />, "info"],
    ["TaskStateBadge AWAITING_APPROVAL", () => <TaskStateBadge state="AWAITING_APPROVAL" />, "warning"],
    ["TaskStateBadge BLOCKED", () => <TaskStateBadge state="BLOCKED" />, "warning"],
    ["TaskStateBadge COMPLETED", () => <TaskStateBadge state="COMPLETED" />, "success"],
    ["TaskStateBadge FAILED", () => <TaskStateBadge state="FAILED" />, "danger"],
    ["TaskStateBadge CANCELLED", () => <TaskStateBadge state="CANCELLED" />, "neutral"],
    ["StepStateBadge WAITING", () => <StepStateBadge state="WAITING" />, "warning"],
    ["StepStateBadge SUCCEEDED", () => <StepStateBadge state="SUCCEEDED" />, "success"],
    ["AttemptStateBadge UNKNOWN", () => <AttemptStateBadge state="UNKNOWN" />, "warning"],
    ["AttemptStateBadge FAILED", () => <AttemptStateBadge state="FAILED" />, "danger"],
    ["StatusBadge pending", () => <StatusBadge status="pending" />, "warning"],
    ["StatusBadge approved", () => <StatusBadge status="approved" />, "success"],
    ["AvailabilityBadge online", () => <AvailabilityBadge availability="online" />, "success"],
    ["AvailabilityBadge unreachable", () => <AvailabilityBadge availability="unreachable" />, "warning"],
    ["StateBadge rejected", () => <StateBadge state="rejected" />, "danger"],
  ])("%s has the %s tone", async (_name, ui, tone) => {
    const { container } = await renderWithI18n(ui());
    expect(container.querySelector(".badge")).toHaveClass(`tone-${tone}`);
  });
});
