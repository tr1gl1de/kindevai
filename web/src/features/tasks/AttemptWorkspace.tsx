import { useTranslation } from "react-i18next";
import type { InstructionChange, WorkspaceChanges, WorkspacePrepared } from "../../api/types";
import { ErrorState } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatBytes } from "../../ui/format";
import { useAttemptWorkspace } from "./api";

interface Props {
  taskId: string;
  stepId: string;
  fence: number;
  number: number;
  live: boolean;
}

const short = (sha: string) => sha.slice(0, 12);
const statuses = ["A", "M", "D", "T"] as const;

/**
 * An attempt's worktree as the runner reported it (KIN-29/70): branch, base and head commits, the repository's
 * instruction files and the changed files. Paths and hashes are the runner's (untrusted) data: rendered as text only.
 */
export function AttemptWorkspace({ taskId, stepId, fence, number, live }: Props) {
  const { t } = useTranslation();
  const q = useAttemptWorkspace(taskId, stepId, fence, live);
  if (q.isPending)
    return (
      <p className="muted" aria-busy="true">
        {t("tasks.wsLoading")}
      </p>
    );
  if (q.isError) return <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />;
  const { prepared, changes } = q.data;
  if (!prepared && !changes) return <p className="muted">{t("tasks.wsNone")}</p>;
  const branch = changes?.branch || prepared?.branch;
  const baseSha = prepared?.baseSha || changes?.baseSha;
  return (
    <section className="attempt-workspace" aria-label={t("tasks.wsShow", { n: number })}>
      <dl className="details">
        {prepared?.repository && (
          <>
            <dt>{t("tasks.repository")}</dt>
            <dd>
              <code>{prepared.repository}</code>
            </dd>
          </>
        )}
        {branch && (
          <>
            <dt>{t("tasks.wsBranch")}</dt>
            <dd>
              <code>{branch}</code>
            </dd>
          </>
        )}
        {prepared?.base && (
          <>
            <dt>{t("tasks.wsBaseBranch")}</dt>
            <dd>
              <code>{prepared.base}</code>
            </dd>
          </>
        )}
        {baseSha && (
          <>
            <dt>{t("tasks.wsBaseCommit")}</dt>
            <dd>
              <code>{short(baseSha)}</code>
            </dd>
          </>
        )}
        <dt>{t("tasks.wsHeadCommit")}</dt>
        <dd>
          {changes?.headSha ? (
            <code>{short(changes.headSha)}</code>
          ) : (
            <span className="muted">{live ? t("tasks.wsHeadPending") : t("tasks.wsHeadNone")}</span>
          )}
        </dd>
      </dl>
      {prepared && <Instructions prepared={prepared} changed={changes?.instructionsChanged ?? []} number={number} />}
      <Changes changes={changes} number={number} live={live} />
    </section>
  );
}

interface Row {
  path: string;
  sha256: string;
  size?: number;
  symlink?: boolean;
  note?: "wsInstrChanged" | "wsInstrAdded" | "wsInstrRemoved";
}

/** The manifest at the start of the attempt, with what changed during it marked in words (never colour alone). */
function instructionRows(prepared: WorkspacePrepared, changed: InstructionChange[]): Row[] {
  const byPath = new Map(changed.map((c) => [c.path, c]));
  const rows: Row[] = prepared.instructions.map((f) => {
    const c = byPath.get(f.path);
    return { ...f, note: c ? (c.after ? "wsInstrChanged" : "wsInstrRemoved") : undefined };
  });
  const known = new Set(prepared.instructions.map((f) => f.path));
  for (const c of changed) {
    if (!known.has(c.path) && c.after) rows.push({ path: c.path, sha256: c.after, note: "wsInstrAdded" });
  }
  return rows;
}

function Instructions({ prepared, changed, number }: { prepared: WorkspacePrepared; changed: InstructionChange[]; number: number }) {
  const { t, i18n } = useTranslation();
  const rows = instructionRows(prepared, changed);
  return (
    <>
      <h5>{t("tasks.wsInstructionsTitle")}</h5>
      <p className="muted">{t("tasks.wsInstructionsHint")}</p>
      {rows.length === 0 ? (
        <p className="muted">{t("tasks.wsInstructionsEmpty")}</p>
      ) : (
        <div className="table-wrap">
          <table>
            <caption className="visually-hidden">{t("tasks.wsInstructionsCaption", { n: number })}</caption>
            <thead>
              <tr>
                <th scope="col">{t("tasks.wsColPath")}</th>
                <th scope="col">{t("tasks.wsColSha")}</th>
                <th scope="col">{t("tasks.wsColSize")}</th>
                <th scope="col">{t("tasks.wsColNotes")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={`${i}:${r.path}`} className={r.note ? "ws-changed" : undefined}>
                  <td className="ws-path">
                    <code>{r.path}</code>
                  </td>
                  <td>
                    <code>{short(r.sha256)}</code>
                  </td>
                  <td>{r.size === undefined ? "—" : formatBytes(r.size, i18n.language)}</td>
                  <td>
                    {r.symlink && <span className="badge">{t("tasks.wsSymlink")}</span>}
                    {r.note && <strong className="badge ws-note">{t(`tasks.${r.note}`)}</strong>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {prepared.instructionsTruncated && <p className="muted">{t("tasks.wsInstructionsTruncated")}</p>}
    </>
  );
}

function Changes({ changes, number, live }: { changes: WorkspaceChanges | null; number: number; live: boolean }) {
  const { t } = useTranslation();
  if (!changes) {
    return (
      <>
        <h5>{t("tasks.wsChangesTitle")}</h5>
        <p className="muted">{live ? t("tasks.wsChangesPending") : t("tasks.wsChangesNone")}</p>
      </>
    );
  }
  const counts = statuses
    .map((s) => [s, changes.files.filter((f) => f.status === s).length] as const)
    .filter(([, n]) => n > 0)
    .map(([s, n]) => t(`tasks.wsCount_${s}`, { n }))
    .join(" · ");
  const known = (s: string): s is (typeof statuses)[number] => (statuses as readonly string[]).includes(s);
  return (
    <>
      <h5>{t("tasks.wsChangesTitle")}</h5>
      {changes.files.length === 0 ? (
        <p className="muted">{t("tasks.wsChangesEmpty")}</p>
      ) : (
        <>
          <p>{counts}</p>
          <ul className="ws-files" aria-label={t("tasks.wsChangesCaption", { n: number })}>
            {changes.files.map((f, i) => (
              <li key={`${i}:${f.path}`}>
                <span className={`badge ws-status ws-status-${known(f.status) ? f.status : "other"}`}>
                  {known(f.status) ? t(`tasks.wsStatus_${f.status}`) : f.status}
                </span>{" "}
                <code className="ws-path">{f.path}</code>
              </li>
            ))}
          </ul>
        </>
      )}
      {(changes.truncated || changes.filesTotal > changes.files.length) && (
        <p className="muted">{t("tasks.wsChangesTruncated", { shown: changes.files.length, total: changes.filesTotal })}</p>
      )}
      {changes.files.length > 0 && <p className="muted">{t("tasks.wsRenames")}</p>}
    </>
  );
}
