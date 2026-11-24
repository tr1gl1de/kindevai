import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router";
import { PageHeading } from "../../ui/feedback";
import { AuthLayout } from "./AuthLayout";
import { ChangePasswordForm } from "./ChangePasswordForm";
import { useLogout } from "./api";

/** Shown instead of the app while the account still has a temporary password. */
export function ForcedPasswordChange() {
  const { t } = useTranslation();
  const logout = useLogout();
  const navigate = useNavigate();
  return (
    <AuthLayout>
      <PageHeading>{t("account.forcedTitle")}</PageHeading>
      <p>{t("account.forcedIntro")}</p>
      <ChangePasswordForm temporary onDone={() => navigate("/projects")} />
      <p>
        <button type="button" className="button subtle" onClick={() => logout.mutate()}>
          {t("login.signOut")}
        </button>
      </p>
    </AuthLayout>
  );
}
