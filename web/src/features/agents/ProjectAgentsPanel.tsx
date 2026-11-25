import { useState } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { AgentCli } from "../../api/types";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useNotify } from "../../ui/notify";
import { useProjectAgents, useSetAgentSwitch } from "./api";
import { BindingsEditor } from "./BindingsEditor";

/** A project's AI CLI allowlist and role binding overrides (Owner or the project's admin). */
export function ProjectAgentsPanel({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const settings = useProjectAgents(projectId);
  const toggle = useSetAgentSwitch({ projectId });
  const notify = useNotify();
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
      setError(
        isApiError(err, "disabled_globally")
          ? t("agents.err_disabled_globally")
          : errorMessage(t, err),
      );
    }
  }

  return (
    <section aria-labelledby="project-agents-title" className="page-section">
      <h2 id="project-agents-title">{t("agents.projectTitle")}</h2>
      <p className="muted">{t("agents.projectIntro")}</p>
      {settings.isPending ? (
        <Loading />
      ) : settings.isError ? (
        <ErrorState
          message={errorMessage(t, settings.error)}
          onRetry={() => void settings.refetch()}
        />
      ) : (
        <>
          <FormAlert message={error} />
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">{t("agents.cliColumn")}</th>
                  <th scope="col">{t("agents.globalColumn")}</th>
                  <th scope="col">{t("agents.projectColumn")}</th>
                  <th scope="col">{t("agents.actionsColumn")}</th>
                </tr>
              </thead>
              <tbody>
                {settings.data.agents.map((a) => {
                  const cli = t(`agents.cli_${a.agent}`);
                  const blocked = !a.global && !a.project.enabled;
                  return (
                    <tr key={a.agent}>
                      <th scope="row">{cli}</th>
                      <td>
                        {t(
                          a.global
                            ? "agents.enabled"
                            : "agents.disabledGlobally",
                        )}
                      </td>
                      <td>
                        <span className="badge">
                          {t(a.effective ? "agents.inUse" : "agents.notInUse")}
                        </span>
                      </td>
                      <td>
                        {blocked ? (
                          <span className="muted">{t("agents.ownerOnly")}</span>
                        ) : (
                          <button
                            type="button"
                            className="button subtle"
                            disabled={toggle.isPending}
                            onClick={() =>
                              void flip(a.agent, !a.project.enabled)
                            }
                          >
                            {t(
                              a.project.enabled
                                ? "agents.disableInProject"
                                : "agents.enableInProject",
                              { cli },
                            )}
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <h3>{t("agents.bindingsTitle")}</h3>
          <p className="muted">{t("agents.projectBindingsIntro")}</p>
          <BindingsEditor
            scope={{ projectId }}
            bindings={settings.data.bindings}
          />
        </>
      )}
    </section>
  );
}
