import { useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { AuthLayout } from "../identity/AuthLayout";
import { useConfirmLink, useLinkPreview } from "./api";
import "./telegram.css";

/** Mini App side of "Link Telegram": shows the target account, confirms, then shows the code for the website. */
export function LinkConfirmPage({ initData, onDone }: { initData: string; onDone: () => void }) {
  const { t, i18n } = useTranslation();
  const preview = useLinkPreview(initData);
  const confirm = useConfirmLink();
  const codeHeading = useRef<HTMLHeadingElement>(null);
  const code = confirm.data?.code;
  useEffect(() => {
    if (code) codeHeading.current?.focus();
  }, [code]);

  return (
    <AuthLayout>
      <PageHeading>{t("telegram.linkTitle")}</PageHeading>
      {preview.isPending && <Loading />}
      {preview.isError && <ErrorState message={errorMessage(t, preview.error)} />}
      {preview.data && !code && (
        <>
          <p>{t("telegram.linkOffer", { login: preview.data.login })}</p>
          <p className="muted">{t("telegram.linkWarning")}</p>
          <FormAlert message={confirm.isError ? errorMessage(t, confirm.error) : undefined} />
          <div className="form-actions">
            <button type="button" className="button primary" disabled={confirm.isPending} onClick={() => confirm.mutate(initData)}>
              {confirm.isPending ? t("form.submitting") : t("telegram.linkConfirm", { login: preview.data.login })}
            </button>
          </div>
        </>
      )}
      {code && confirm.data && (
        <section className="card secret" aria-labelledby="tg-code">
          <h2 id="tg-code" ref={codeHeading} tabIndex={-1}>
            {t("telegram.codeTitle")}
          </h2>
          <p className="secret-value tg-code">
            {code}
          </p>
          <p>{t("telegram.codeText", { login: confirm.data.login, date: formatDateTime(confirm.data.expiresAt, i18n.language) })}</p>
          <p className="muted">{t("telegram.codeThen")}</p>
          <div className="form-actions">
            <button type="button" className="button" onClick={onDone}>
              {t("telegram.codeEntered")}
            </button>
          </div>
        </section>
      )}
    </AuthLayout>
  );
}
