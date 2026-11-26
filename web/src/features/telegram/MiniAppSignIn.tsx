import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { ErrorState, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { AuthLayout } from "../identity/AuthLayout";
import { LoginPage } from "../identity/LoginPage";
import { useMiniAppLogin } from "./api";

/** Signs in from the Mini App with Telegram's signed initData; explains what to do if the account is not linked. */
export function MiniAppSignIn({ initData }: { initData: string }) {
  const { t } = useTranslation();
  const login = useMiniAppLogin();
  const [withPassword, setWithPassword] = useState(false);
  const started = useRef(false);
  useEffect(() => {
    if (started.current) return; // once per start, also under StrictMode
    started.current = true;
    login.mutate(initData);
  }, [initData, login]);

  if (withPassword) return <LoginPage />;
  const notLinked = isApiError(login.error, "telegram_not_linked");
  return (
    <AuthLayout>
      <PageHeading>{notLinked ? t("telegram.notLinkedTitle") : t("telegram.signInTitle")}</PageHeading>
      {(login.isPending || login.isIdle) && <Loading label={t("telegram.signingIn")} />}
      {notLinked && (
        <>
          <p>{t("telegram.notLinkedText")}</p>
          <div className="form-actions">
            <button type="button" className="button primary" onClick={() => setWithPassword(true)}>
              {t("telegram.signInWithPassword")}
            </button>
          </div>
        </>
      )}
      {login.isError && !notLinked && (
        <ErrorState
          message={errorMessage(t, login.error)}
          onRetry={isApiError(login.error, "telegram_auth_expired") ? undefined : () => login.mutate(initData)}
        />
      )}
    </AuthLayout>
  );
}
