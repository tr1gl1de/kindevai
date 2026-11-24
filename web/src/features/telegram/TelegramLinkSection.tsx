import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { CopyButton } from "../../ui/CopyButton";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { useCancelLink, useCompleteLink, useStartLink, useTelegramLink, useUnlink } from "./api";

const fields = ["code"] as const;

/** Account page: link this account to Telegram (two-sided confirmation) or unlink it. */
export function TelegramLinkSection() {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const status = useTelegramLink();
  const start = useStartLink();
  const cancel = useCancelLink();
  const complete = useCompleteLink();
  const unlink = useUnlink();
  const [url, setUrl] = useState<string>();
  const [code, setCode] = useState("");
  const [formError, setFormError] = useState<string>();
  const [confirmUnlink, setConfirmUnlink] = useState(false);
  const { errors, show, clear, register } = useFieldErrors(fields);

  async function onStart() {
    setFormError(undefined);
    try {
      const r = await start.mutateAsync();
      setUrl(r.url);
    } catch (err) {
      setFormError(errorMessage(t, err));
    }
  }

  async function onComplete(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const c = code.replace(/\s+/g, "");
    if (!/^\d{6}$/.test(c)) {
      show({ code: t("telegram.codeFormat") });
      return;
    }
    try {
      await complete.mutateAsync(c);
      setCode("");
      setUrl(undefined);
      notify(t("telegram.linked"));
    } catch (err) {
      if (isApiError(err, "link_code_wrong")) show({ code: t("telegram.codeWrong") });
      else setFormError(errorMessage(t, err));
    }
  }

  const heading = (
    <h2 id="tg-section" tabIndex={-1}>
      {t("telegram.sectionTitle")}
    </h2>
  );
  if (status.isPending)
    return (
      <section className="card" aria-labelledby="tg-section">
        {heading}
        <Loading />
      </section>
    );
  if (status.isError)
    return (
      <section className="card" aria-labelledby="tg-section">
        {heading}
        <ErrorState message={errorMessage(t, status.error)} onRetry={() => void status.refetch()} />
      </section>
    );
  const { configured, identity, request } = status.data;
  const state = request?.state;

  return (
    <section className="card" aria-labelledby="tg-section">
      {heading}
      <FormAlert message={formError} />
      {identity ? (
        <>
          <p>
            {t("telegram.linkedAs", {
              name: identity.username ? `@${identity.username}` : identity.telegramId,
              date: formatDateTime(identity.linkedAt, i18n.language),
            })}
          </p>
          <div className="form-actions">
            <button type="button" className="button danger" onClick={() => setConfirmUnlink(true)}>
              {t("telegram.unlink")}
            </button>
          </div>
          {confirmUnlink && (
            <ConfirmDialog
              title={t("telegram.unlinkConfirmTitle")}
              text={t("telegram.unlinkConfirmText")}
              confirmLabel={t("telegram.unlink")}
              cancelLabel={t("telegram.keepLinked")}
              busy={unlink.isPending}
              onCancel={() => setConfirmUnlink(false)}
              onConfirm={() =>
                unlink.mutate(undefined, {
                  onSuccess: () => notify(t("telegram.unlinked")),
                  onError: (err) => setFormError(errorMessage(t, err)),
                  onSettled: () => setConfirmUnlink(false),
                })
              }
            />
          )}
        </>
      ) : !configured ? (
        <p className="muted">{t("telegram.notConfigured")}</p>
      ) : state === "awaiting_code" ? (
        <>
          <p>{t("telegram.confirmedBy", { name: request?.telegramName ?? "" })}</p>
          <form noValidate onSubmit={onComplete} aria-label={t("telegram.codeFormTitle")}>
            <TextField
              ref={register("code")}
              label={t("telegram.code")}
              hint={t("telegram.codeHint")}
              error={errors.code}
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={7}
              value={code}
              onChange={(e) => {
                setCode(e.target.value);
                clear("code");
              }}
            />
            <div className="form-actions">
              <button type="submit" className="button primary" disabled={complete.isPending}>
                {complete.isPending ? t("form.submitting") : t("telegram.finishLink")}
              </button>
              <button type="button" className="button" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
                {t("telegram.notMe")}
              </button>
            </div>
          </form>
        </>
      ) : state === "awaiting_telegram" && url ? (
        <>
          <p>{t("telegram.openLink", { date: formatDateTime(request!.expiresAt, i18n.language) })}</p>
          <p className="secret-value link">
            <a href={url} target="_blank" rel="noopener noreferrer">
              {url}
            </a>
          </p>
          <div className="form-actions">
            <CopyButton text={url} failedHint={t("telegram.copyFailed")} />
            <button type="button" className="button" disabled={cancel.isPending} onClick={() => cancel.mutate(undefined, { onSuccess: () => setUrl(undefined) })}>
              {t("telegram.cancelLink")}
            </button>
          </div>
          <Loading label={t("telegram.waiting")} />
        </>
      ) : (
        <>
          <p>{state === "expired" || state === "cancelled" ? t(`telegram.request_${state}`) : t("telegram.intro")}</p>
          <div className="form-actions">
            <button type="button" className="button primary" disabled={start.isPending} onClick={() => void onStart()}>
              {start.isPending ? t("form.submitting") : t("telegram.startLink")}
            </button>
          </div>
        </>
      )}
    </section>
  );
}
