import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import type { NotificationPreferences, NotificationType } from "../../api/types";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useNotify } from "../../ui/notify";
import { useNotificationPreferences, useSaveNotificationPreferences } from "./api";
import "./alerts.css";

const allTypes: NotificationType[] = ["approval_requested", "alert_raised", "task_finished"];

/** Account page: which Telegram notifications this user receives and in which language. */
export function NotificationsSection() {
  const { t } = useTranslation();
  const prefs = useNotificationPreferences();
  return (
    <section aria-labelledby="notifications-title">
      <h2 id="notifications-title">{t("notifications.title")}</h2>
      {prefs.isPending ? (
        <Loading />
      ) : prefs.isError ? (
        <ErrorState message={errorMessage(t, prefs.error)} onRetry={() => void prefs.refetch()} />
      ) : (
        <PreferencesForm initial={prefs.data} />
      )}
    </section>
  );
}

function PreferencesForm({ initial }: { initial: NotificationPreferences }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveNotificationPreferences();
  const [draft, setDraft] = useState<NotificationPreferences>({ language: initial.language, disabled: initial.disabled });
  const [error, setError] = useState<string>();
  const types = initial.types ?? allTypes;

  function toggle(type: NotificationType, on: boolean) {
    setDraft((d) => ({ ...d, disabled: on ? d.disabled.filter((x) => x !== type) : [...d.disabled, type] }));
  }

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setError(undefined);
    try {
      await save.mutateAsync(draft);
      notify(t("notifications.saved"));
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  return (
    <>
      <p className="muted">{t(initial.telegramLinked ? "notifications.intro" : "notifications.notLinked")}</p>
      <form onSubmit={(ev) => void onSubmit(ev)} noValidate>
        <FormAlert message={error} />
        <fieldset className="checkbox-list">
          <legend>{t("notifications.typesLegend")}</legend>
          {types.map((type) => (
            <label key={type}>
              <input type="checkbox" checked={!draft.disabled.includes(type)} onChange={(e) => toggle(type, e.target.checked)} />{" "}
              {t(`notifications.type_${type}`)}
            </label>
          ))}
        </fieldset>
        <div className="field">
          <label htmlFor="notifications-language">{t("notifications.language")}</label>
          <select
            id="notifications-language"
            value={draft.language}
            onChange={(e) => setDraft({ ...draft, language: e.target.value === "ru" ? "ru" : "en" })}
          >
            <option value="en">English</option>
            <option value="ru">Русский</option>
          </select>
        </div>
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("notifications.save")}
        </button>
      </form>
    </>
  );
}
