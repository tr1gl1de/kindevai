import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import type { MaintenanceStatus } from "../../api/types";
import { useMaintenance, useSetMaintenance } from "./api";

/** Owner: maintenance mode before moving or backing up the installation (KIN-41). */
export function MaintenancePage() {
  const { t } = useTranslation();
  const st = useMaintenance();
  return (
    <div className="narrow">
      <PageHeading>{t("maintenance.title")}</PageHeading>
      <p>{t("maintenance.intro")}</p>
      {st.isPending && <Loading />}
      {st.isError && <ErrorState message={errorMessage(t, st.error)} onRetry={() => void st.refetch()} />}
      {st.data && <MaintenanceForm status={st.data} />}
      <h2>{t("maintenance.backupTitle")}</h2>
      <p>{t("maintenance.backupText")}</p>
      <pre className="command">ai-platform export --out backup.tar.gz --key-out backup.key</pre>
    </div>
  );
}

function MaintenanceForm({ status }: { status: MaintenanceStatus }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const set = useSetMaintenance();
  const [reason, setReason] = useState(status.reason);
  const [reasonError, setReasonError] = useState<string>();
  const [formError, setFormError] = useState<string>();

  async function submit(enabled: boolean) {
    setFormError(undefined);
    setReasonError(undefined);
    if (enabled && reason.trim().length > 500) {
      setReasonError(t("validation.reason_invalid"));
      return;
    }
    try {
      await set.mutateAsync({ enabled, reason: enabled ? reason.trim() : "" });
      notify(enabled ? t("maintenance.enabled") : t("maintenance.disabled"));
    } catch (err) {
      if (isApiError(err, "validation")) setReasonError(t("validation.reason_invalid"));
      else setFormError(errorMessage(t, err));
    }
  }

  function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    void submit(true);
  }

  return (
    <>
      <p>
        {status.enabled
          ? t("maintenance.stateOn", {
              date: status.since ? formatDateTime(status.since, i18n.language) : "",
              by: status.by,
            })
          : t("maintenance.stateOff")}
      </p>
      {status.enabled && <p>{t("maintenance.draining", { count: status.activeAttempts })}</p>}
      <FormAlert message={formError} />
      <form noValidate onSubmit={onSubmit} aria-label={t("maintenance.title")}>
        <TextField
          label={t("maintenance.reason")}
          hint={t("maintenance.reasonHint")}
          error={reasonError}
          value={reason}
          maxLength={500}
          onChange={(e) => {
            setReason(e.target.value);
            setReasonError(undefined);
          }}
        />
        <div className="form-actions">
          <button type="submit" className="button primary" disabled={set.isPending}>
            {status.enabled ? t("maintenance.updateReason") : t("maintenance.turnOn")}
          </button>
          {status.enabled && (
            <button type="button" className="button" disabled={set.isPending} onClick={() => void submit(false)}>
              {t("maintenance.turnOff")}
            </button>
          )}
        </div>
      </form>
    </>
  );
}
