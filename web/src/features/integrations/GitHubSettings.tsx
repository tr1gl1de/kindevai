import type { TFunction } from "i18next";
import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { ConnectionCheck, GitHubApp } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { TextAreaField, TextField } from "../../ui/Field";
import { FormAlert } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { useCheckGitHub, useDisconnectGitHub, useSaveGitHub } from "./api";
import { fieldError } from "./IntegrationsPage";

/** Failure codes of the GitHub client with their own message; others read as "unknown". */
const knownCodes = [
  "unauthorized",
  "forbidden",
  "not_found",
  "not_installed",
  "unreachable",
  "redirected",
  "unexpected_status",
  "bad_response",
  "bad_key",
  "timeout",
  "not_configured",
];

/** Text of a GitHub check or repository read failure. */
export function githubFailure(t: TFunction, check: ConnectionCheck): string {
  const code = check.code ?? "unknown";
  let text = t(`github.code_${knownCodes.includes(code) ? code : "unknown"}` as "github.code_unknown");
  if (check.detail && code !== "not_installed") text += ` (${check.detail})`;
  return text;
}

function AppCheck({ check }: { check: ConnectionCheck }) {
  const { t, i18n } = useTranslation();
  let text: string;
  switch (check.status) {
    case "pending":
      text = t("github.checkPending");
      break;
    case "ok":
      text = t("github.checkOk", { slug: check.detail ?? "" });
      break;
    case "failed":
      text = githubFailure(t, check);
      break;
    default:
      text = t("integrations.checkNone");
  }
  return (
    <p className="check-result" aria-live="polite" aria-busy={check.status === "pending"}>
      <strong>{t(`integrations.checkLabel_${check.status}`)}</strong> {text}
      {check.checkedAt && check.status !== "pending" && (
        <span className="muted"> {t("integrations.checkedAt", { date: formatDateTime(check.checkedAt, i18n.language) })}</span>
      )}
    </p>
  );
}

/** How to create the App: webhook URL, minimal permissions and events. */
function SetupGuide() {
  const { t } = useTranslation();
  const webhookUrl = `${window.location.origin}/api/webhooks/github`;
  return (
    <details className="card">
      <summary>{t("github.guideTitle")}</summary>
      <ol>
        <li>{t("github.guideCreate")}</li>
        <li>
          {t("github.guideWebhook")} <span className="code-value">{webhookUrl}</span>
        </li>
        <li>
          {t("github.guidePermissions")}
          <ul>
            <li>{t("github.permContents")}</li>
            <li>{t("github.permPullRequests")}</li>
            <li>{t("github.permChecks")}</li>
            <li>{t("github.permMetadata")}</li>
          </ul>
          {t("github.permNot")}
        </li>
        <li>{t("github.guideEvents")}</li>
        <li>{t("github.guideInstall")}</li>
        <li>{t("github.guideRuleset")}</li>
      </ol>
    </details>
  );
}

const ghFields = ["appId", "privateKey", "webhookSecret"] as const;

/** Owner-only: the installation's GitHub App (App ID, private key, webhook secret; write-only credentials). */
export function GitHubSettings({ app }: { app: GitHubApp | null }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const save = useSaveGitHub();
  const check = useCheckGitHub();
  const disconnect = useDisconnectGitHub();
  const { errors, show, clear, register, count } = useFieldErrors(ghFields);
  const [appId, setAppId] = useState(app ? String(app.appId) : "");
  const [privateKey, setPrivateKey] = useState("");
  const [webhookSecret, setWebhookSecret] = useState("");
  const [formError, setFormError] = useState<string>();
  const [confirm, setConfirm] = useState(false);

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const missing: Partial<Record<(typeof ghFields)[number], string>> = {};
    if (!appId.trim()) missing.appId = t("form.required");
    if (!app && !privateKey.trim()) missing.privateKey = t("form.required");
    if (!app && !webhookSecret.trim()) missing.webhookSecret = t("form.required");
    if (Object.keys(missing).length) {
      show(missing);
      return;
    }
    try {
      await save.mutateAsync({ appId: appId.trim(), privateKey: privateKey.trim(), webhookSecret: webhookSecret.trim() });
      setPrivateKey("");
      setWebhookSecret("");
      save.reset(); // drop the request (with the key) from the mutation cache
      notify(t("github.saved"));
    } catch (err) {
      save.reset();
      const f = isApiError(err) ? err.field : undefined;
      if (isApiError(err, "validation") && (f === "appId" || f === "privateKey" || f === "webhookSecret")) show({ [f]: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  async function runCheck() {
    setFormError(undefined);
    try {
      await check.mutateAsync();
    } catch (err) {
      setFormError(errorMessage(t, err));
    }
  }

  async function doDisconnect() {
    setFormError(undefined);
    try {
      await disconnect.mutateAsync();
      notify(t("github.disconnected"));
    } catch (err) {
      setFormError(errorMessage(t, err));
    } finally {
      setConfirm(false);
    }
  }

  const setHint = (meta: GitHubApp["privateKey"] | undefined, unset: string) =>
    meta?.set ? t("github.secretSet", { date: meta.updatedAt ? formatDateTime(meta.updatedAt, i18n.language) : "" }) : unset;

  return (
    <>
      {app ? (
        <>
          <p>{t("github.connected", { id: app.appId })}</p>
          <AppCheck check={app.check} />
          <p className="muted">
            {app.lastDeliveryAt
              ? t("github.lastDelivery", { event: app.lastEvent ?? "", date: formatDateTime(app.lastDeliveryAt, i18n.language) })
              : t("github.noDelivery")}
          </p>
        </>
      ) : (
        <p>
          <strong>{t("github.notConfigured")}</strong>
        </p>
      )}
      <SetupGuide />
      <form noValidate onSubmit={onSubmit} aria-label={t("github.form")} className="card form-card">
        <FormAlert message={count ? t("form.summary", { count }) : formError} />
        <TextField
          ref={register("appId")}
          label={t("github.appId")}
          hint={t("github.appIdHint")}
          error={errors.appId}
          inputMode="numeric"
          autoComplete="off"
          value={appId}
          onChange={(e) => {
            setAppId(e.target.value);
            clear("appId");
          }}
        />
        <TextAreaField
          ref={register("privateKey")}
          label={t("github.privateKey")}
          hint={setHint(app?.privateKey, t("github.privateKeyHint"))}
          keepHint
          error={errors.privateKey}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          rows={4}
          value={privateKey}
          onChange={(e) => {
            setPrivateKey(e.target.value);
            clear("privateKey");
          }}
        />
        <TextField
          ref={register("webhookSecret")}
          label={t("github.webhookSecret")}
          hint={setHint(app?.webhookSecret, t("github.webhookSecretHint"))}
          keepHint
          error={errors.webhookSecret}
          type="password"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          value={webhookSecret}
          onChange={(e) => {
            setWebhookSecret(e.target.value);
            clear("webhookSecret");
          }}
        />
        <div className="form-actions">
          <button type="submit" className="button primary" disabled={save.isPending}>
            {save.isPending ? t("form.submitting") : t("github.save")}
          </button>
          {app && (
            <>
              <button type="button" className="button" onClick={() => void runCheck()} disabled={check.isPending || app.check.status === "pending"}>
                {t("github.check")}
              </button>
              <button type="button" className="button subtle" onClick={() => setConfirm(true)}>
                {t("integrations.disconnect")}
              </button>
            </>
          )}
        </div>
      </form>
      {confirm && (
        <ConfirmDialog
          title={t("github.disconnectTitle")}
          text={t("github.disconnectText")}
          confirmLabel={t("integrations.disconnect")}
          cancelLabel={t("users.cancel")}
          busy={disconnect.isPending}
          onConfirm={() => void doDisconnect()}
          onCancel={() => setConfirm(false)}
        />
      )}
    </>
  );
}
