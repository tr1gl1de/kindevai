import { useState } from "react";
import { useTranslation } from "react-i18next";
import { PageHeading } from "../../ui/feedback";
import { useNotify } from "../../ui/notify";
import { ChangePasswordForm } from "./ChangePasswordForm";

export function AccountPage() {
  const { t } = useTranslation();
  const notify = useNotify();
  const [round, setRound] = useState(0); // remount the form to clear the fields after a change
  return (
    <div className="narrow">
      <PageHeading>{t("account.title")}</PageHeading>
      <ChangePasswordForm
        key={round}
        temporary={false}
        onDone={() => {
          notify(t("account.changed"));
          setRound((r) => r + 1);
        }}
      />
    </div>
  );
}
