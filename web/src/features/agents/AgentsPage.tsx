import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { ApiError } from "../../api/client";
import type { AgentCli, AgentSettings } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import {
  EmptyState,
  ErrorState,
  FormAlert,
  Loading,
  PageHeading,
} from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import {
  useAgentSettings,
  useEmergencyStop,
  useMarkCLIReady,
  useSetAgentSwitch,
} from "./api";
import { LimitsSection } from "../planDiscussion/LimitsSection";
import { BindingsEditor } from "./BindingsEditor";

/** Agent Management (Owner): global CLI switches, emergency stop, global role bindings and CLI cool-downs. */
export function AgentsPage() {
  const { t } = useTranslation();
  const settings = useAgentSettings();
  return (
    <>
      <PageHeading>{t("agents.title")}</PageHeading>
      <p className="muted">{t("agents.intro")}</p>
      {settings.isPending ? (
        <Loading />
      ) : settings.isError ? (
        <ErrorState
          message={errorMessage(t, settings.error)}
          onRetry={() => void settings.refetch()}
        />
      ) : (
        <>
          <Switches settings={settings.data} />
          <section aria-labelledby="bindings-title" className="page-section">
            <h2 id="bindings-title">{t("agents.bindingsTitle")}</h2>
            <p className="muted">{t("agents.bindingsIntro")}</p>
            <BindingsEditor scope={{}} bindings={settings.data.bindings} />
          </section>
          <Cooldowns settings={settings.data} />
          <LimitsSection />
        </>
      )}
    </>
  );
}

function Switches({ settings }: { settings: AgentSettings }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const toggle = useSetAgentSwitch({});
  const stop = useEmergencyStop();
  const [confirm, setConfirm] = useState<AgentCli>();
  const [error, setError] = useState<string>();

  async function flip(agent: AgentCli, enabled: boolean) {
    setError(undefined);
    try {
      await toggle.mutateAsync({ agent, enabled });
      notify(
        t(enabled ? "agents.enabledNotice" : "agents.disabledNotice", {
          cli: t(`agents.cli_${agent}`),
        }),
      );
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  async function emergency(agent: AgentCli) {
    setError(undefined);
    try {
      const r = await stop.mutateAsync(agent);
      notify(
        t("agents.stoppedNotice", {
          cli: t(`agents.cli_${agent}`),
          count: r.stopped,
        }),
      );
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setConfirm(undefined);
    }
  }

  return (
    <section aria-labelledby="clis-title" className="page-section">
      <h2 id="clis-title">{t("agents.clisTitle")}</h2>
      <p className="muted">{t("agents.clisIntro")}</p>
      <FormAlert message={error} />
      <div className="table-wrap">
        <table className="agents-table">
          <thead>
            <tr>
              <th scope="col">{t("agents.cliColumn")}</th>
              <th scope="col">{t("agents.statusColumn")}</th>
              <th scope="col">{t("agents.actionsColumn")}</th>
            </tr>
          </thead>
          <tbody>
            {settings.switches.map((s) => {
              const cli = t(`agents.cli_${s.agent}`);
              return (
                <tr key={s.agent}>
                  <th scope="row">{cli}</th>
                  <td>
                    <span className="badge">
                      {t(s.enabled ? "agents.enabled" : "agents.disabled")}
                    </span>
                    {s.set && s.updatedAt && (
                      <div className="muted">
                        {t("agents.changedBy", {
                          by: s.updatedBy,
                          at: formatDateTime(s.updatedAt, i18n.language),
                        })}
                      </div>
                    )}
                  </td>
                  <td className="row-actions">
                    <button
                      type="button"
                      className="button subtle"
                      disabled={toggle.isPending}
                      onClick={() => void flip(s.agent, !s.enabled)}
                    >
                      {t(s.enabled ? "agents.disable" : "agents.enable", {
                        cli,
                      })}
                    </button>
                    <button
                      type="button"
                      className="button danger"
                      onClick={() => setConfirm(s.agent)}
                    >
                      {t("agents.emergencyStop", { cli })}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {confirm && (
        <ConfirmDialog
          title={t("agents.stopTitle", { cli: t(`agents.cli_${confirm}`) })}
          text={t("agents.stopText", { cli: t(`agents.cli_${confirm}`) })}
          confirmLabel={t("agents.stopConfirm")}
          cancelLabel={t("agents.cancel")}
          busy={stop.isPending}
          onConfirm={() => void emergency(confirm)}
          onCancel={() => setConfirm(undefined)}
        />
      )}
    </section>
  );
}

function Cooldowns({ settings }: { settings: AgentSettings }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const ready = useMarkCLIReady();
  const qc = useQueryClient();
  const invalidate = () => qc.invalidateQueries({ queryKey: ["agents"] });
  const [error, setError] = useState<string>();

  async function markReady(runnerId: string, agent: AgentCli) {
    setError(undefined);
    try {
      await ready.mutateAsync({ runnerId, agent });
      notify(
        t("agents.readyNotice", {
          cli: t(`agents.cli_${agent}`),
          runner: runnerId,
        }),
      );
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        // Already lifted (the CLI worked again or another click won): the list refreshes.
        await invalidate();
        return;
      }
      setError(errorMessage(t, err));
    }
  }

  return (
    <section aria-labelledby="cooldowns-title" className="page-section">
      <h2 id="cooldowns-title">{t("agents.cooldownsTitle")}</h2>
      <FormAlert message={error} />
      {settings.cooldowns.length === 0 ? (
        <EmptyState title={t("agents.noCooldowns")}>
          {t("agents.noCooldownsText")}
        </EmptyState>
      ) : (
        <>
          <p className="muted">{t("agents.cooldownsIntro")}</p>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">{t("agents.runnerColumn")}</th>
                  <th scope="col">{t("agents.cliColumn")}</th>
                  <th scope="col">{t("agents.causeColumn")}</th>
                  <th scope="col">{t("agents.untilColumn")}</th>
                  <th scope="col">{t("agents.reasonColumn")}</th>
                  <th scope="col">{t("agents.actionsColumn")}</th>
                </tr>
              </thead>
              <tbody>
                {settings.cooldowns.map((c) => {
                  const cli = t(`agents.cli_${c.agent}`);
                  return (
                    <tr key={`${c.runnerId}/${c.agent}`}>
                      <td>
                        <code>{c.runnerId}</code>
                      </td>
                      <td>{cli}</td>
                      <td>
                        <span className="badge">
                          {t(`agents.cause_${c.cause}`)}
                        </span>
                      </td>
                      <td>
                        {c.parked
                          ? t("agents.waitingForYou", { count: c.strikes })
                          : formatDateTime(c.until, i18n.language)}
                      </td>
                      <td>{c.reason}</td>
                      <td className="row-actions">
                        <button
                          type="button"
                          className="button subtle"
                          disabled={ready.isPending}
                          onClick={() => void markReady(c.runnerId, c.agent)}
                        >
                          {t("agents.markReady", { cli, runner: c.runnerId })}
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}
