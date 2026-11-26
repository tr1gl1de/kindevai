import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import type { EnrollmentToken } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { CopyButton } from "../../ui/CopyButton";
import { EmptyState, ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { enrollCommand, needsHttpsForRemote, useCreateEnrollmentToken, useEnrollmentTokens, useRevokeEnrollmentToken, useRunners } from "./api";
import { StatusBadge } from "./StatusBadge";
import "./runners.css";

/** Owner-only: enrolled machines and the "Add runner" flow with a one-time enrollment token. */
export function RunnersPage() {
  const { t } = useTranslation();
  const create = useCreateEnrollmentToken();
  const [shown, setShown] = useState<{ token: string; expiresAt: string } | null>(null);
  const [error, setError] = useState<string>();

  async function add() {
    setError(undefined);
    try {
      const r = await create.mutateAsync();
      create.reset(); // drop the response (with the token) from the mutation cache
      setShown({ token: r.token, expiresAt: r.enrollmentToken.expiresAt });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  return (
    <>
      <div className="page-head">
        <PageHeading>{t("runners.title")}</PageHeading>
        <button type="button" className="button primary" onClick={() => void add()} disabled={create.isPending || shown !== null}>
          {create.isPending ? t("form.submitting") : t("runners.add")}
        </button>
      </div>
      <p className="muted">{t("runners.intro")}</p>
      <FormAlert message={error} />
      {shown && <TokenCard key={shown.token} {...shown} onDone={() => setShown(null)} />}
      <RunnerList />
      <PendingTokens />
    </>
  );
}

/** One-time display of the token with install steps; the command never contains the token. */
function TokenCard({ token, expiresAt, onDone }: { token: string; expiresAt: string; onDone: () => void }) {
  const { t, i18n } = useTranslation();
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), []);
  const command = enrollCommand();
  return (
    <section className="card secret" aria-label={t("runners.tokenTitle")}>
      <h2 ref={heading} tabIndex={-1}>
        {t("runners.tokenTitle")}
      </h2>
      <p>{t("runners.tokenText", { date: formatDateTime(expiresAt, i18n.language) })}</p>
      <p>
        <code className="secret-value link">{token}</code>
      </p>
      <div className="form-actions">
        <CopyButton text={token} label={t("runners.copyToken")} failedHint={t("runners.copyFailed")} />
      </div>
      <ol className="steps">
        <li>{t("runners.step1")}</li>
        <li>
          {t("runners.step2")}
          <pre className="command">
            <code>{command}</code>
          </pre>
          <CopyButton text={command} label={t("runners.copyCommand")} failedHint={t("runners.copyFailed")} />
        </li>
        <li>{t("runners.step3")}</li>
      </ol>
      {needsHttpsForRemote() && <p className="warning">{t("runners.httpsWarning")}</p>}
      <div className="form-actions">
        <button type="button" className="button primary" onClick={onDone}>
          {t("users.done")}
        </button>
      </div>
    </section>
  );
}

function RunnerList() {
  const { t, i18n } = useTranslation();
  const runners = useRunners();
  return (
    <section aria-labelledby="runners-list" className="page-section">
      <h2 id="runners-list">{t("runners.listTitle")}</h2>
      {runners.isPending && <Loading />}
      {runners.isError && <ErrorState message={errorMessage(t, runners.error)} onRetry={() => void runners.refetch()} />}
      {runners.data?.length === 0 && (
        <EmptyState title={t("runners.empty")}>
          <p className="muted">{t("runners.emptyHint")}</p>
        </EmptyState>
      )}
      {runners.data && runners.data.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("runners.hostname")}</th>
                <th scope="col">{t("runners.status")}</th>
                <th scope="col">{t("runners.fingerprint")}</th>
                <th scope="col">{t("runners.platform")}</th>
                <th scope="col">{t("runners.projects")}</th>
                <th scope="col">{t("runners.enrolled")}</th>
              </tr>
            </thead>
            <tbody>
              {runners.data.map((r) => (
                <tr key={r.id}>
                  <td>
                    <Link to={`/runners/${encodeURIComponent(r.id)}`}>{r.hostname}</Link>
                  </td>
                  <td>
                    <StatusBadge status={r.status} />
                  </td>
                  <td>
                    <code className="fingerprint">{r.fingerprint}</code>
                  </td>
                  <td>{`${r.os}/${r.arch}`}</td>
                  <td>{r.projects.length ? r.projects.map((p) => p.name || p.id).join(", ") : t("runners.noProjects")}</td>
                  <td>{formatDateTime(r.enrolledAt, i18n.language)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/** Tokens that can still enroll a machine, so the Owner can withdraw one handed out by mistake. */
function PendingTokens() {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const tokens = useEnrollmentTokens();
  const revoke = useRevokeEnrollmentToken();
  const [confirm, setConfirm] = useState<EnrollmentToken | null>(null);
  const [error, setError] = useState<string>();
  const pending = tokens.data?.filter((tok) => tok.state === "pending") ?? [];

  async function run(tok: EnrollmentToken) {
    setError(undefined);
    try {
      await revoke.mutateAsync(tok.id);
      notify(t("runners.tokenRevoked"));
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setConfirm(null);
    }
  }

  if (tokens.isError) return <ErrorState message={errorMessage(t, tokens.error)} onRetry={() => void tokens.refetch()} />;
  if (pending.length === 0) return <FormAlert message={error} />;
  return (
    <section aria-labelledby="runner-tokens" className="page-section">
      <h2 id="runner-tokens">{t("runners.tokensTitle")}</h2>
      <FormAlert message={error} />
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th scope="col">{t("runners.tokenState")}</th>
              <th scope="col">{t("runners.tokenExpires")}</th>
              <th scope="col">
                <span className="visually-hidden">{t("runners.revokeToken")}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {pending.map((tok) => (
              <tr key={tok.id}>
                <td>{t(`runners.tokenState_${tok.state}`)}</td>
                <td>{formatDateTime(tok.expiresAt, i18n.language)}</td>
                <td className="row-actions">
                  <button type="button" className="button subtle" onClick={() => setConfirm(tok)}>
                    {t("runners.revokeToken")}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {confirm && (
        <ConfirmDialog
          title={t("runners.revokeTokenTitle")}
          text={t("runners.revokeTokenText")}
          confirmLabel={t("runners.revokeToken")}
          cancelLabel={t("users.cancel")}
          busy={revoke.isPending}
          onConfirm={() => void run(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
    </section>
  );
}
