import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import type { TelegramSettings } from "../../api/types";
import { useSaveTelegramSettings, useTelegramSettings } from "./api";

const fields = ["botUsername", "appShortName", "botToken"] as const;
type Field = (typeof fields)[number];

/** Owner: the bot and Mini App used for Telegram sign-in. The bot token is write-only. */
export function TelegramSettingsPage() {
  const { t } = useTranslation();
  const settings = useTelegramSettings();
  return (
    <div className="narrow">
      <PageHeading>{t("telegram.settingsTitle")}</PageHeading>
      {settings.isPending && <Loading />}
      {settings.isError && (
        <ErrorState
          message={errorMessage(t, settings.error)}
          onRetry={() => void settings.refetch()}
        />
      )}
      {settings.data && <SettingsForm loaded={settings.data} />}
    </div>
  );
}

function SettingsForm({ loaded }: { loaded: TelegramSettings }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const save = useSaveTelegramSettings();
  const { errors, show, clear, register } = useFieldErrors(fields);
  const [values, setValues] = useState({
    botUsername: loaded.botUsername,
    appShortName: loaded.appShortName,
    botToken: "",
  });
  const [formError, setFormError] = useState<string>();

  function validate() {
    const e: Partial<Record<Field, string>> = {};
    if (!/^@?[A-Za-z][A-Za-z0-9_]{1,28}bot$/i.test(values.botUsername.trim()))
      e.botUsername = t("validation.bot_username_format");
    if (!/^[A-Za-z0-9][A-Za-z0-9_]{2,63}$/.test(values.appShortName.trim()))
      e.appShortName = t("validation.app_short_name_format");
    const tok = values.botToken.trim();
    if (tok === "" && !loaded.tokenSet)
      e.botToken = t("validation.bot_token_required");
    else if (tok !== "" && !/^\d{5,20}:[A-Za-z0-9_-]{30,64}$/.test(tok))
      e.botToken = t("validation.bot_token_format");
    return e;
  }

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const e = validate();
    if (Object.keys(e).length) {
      show(e);
      return;
    }
    try {
      await save.mutateAsync({
        botUsername: values.botUsername.trim(),
        appShortName: values.appShortName.trim(),
        botToken: values.botToken.trim(),
      });
      save.reset(); // drop the variables holding the token
      setValues((v) => ({ ...v, botToken: "" }));
      notify(t("telegram.settingsSaved"));
    } catch (err) {
      if (isApiError(err, "validation") && fields.includes(err.field as Field))
        show({
          [err.field as Field]: t(
            `validation.${err.detail}` as "validation.bot_token_format",
          ),
        });
      else setFormError(errorMessage(t, err));
    }
  }

  const set = (f: Field) => (e: { target: { value: string } }) => {
    setValues((v) => ({ ...v, [f]: e.target.value }));
    clear(f);
  };

  return (
    <>
      <p>{t("telegram.settingsIntro")}</p>
      <p className={loaded.configured ? undefined : "muted"}>
        {loaded.configured && loaded.updatedAt
          ? t("telegram.settingsConfigured", {
              date: formatDateTime(loaded.updatedAt, i18n.language),
            })
          : t("telegram.settingsNotConfigured")}
      </p>
      <FormAlert message={formError} />
      <form
        noValidate
        onSubmit={onSubmit}
        aria-label={t("telegram.settingsTitle")}
      >
        <TextField
          ref={register("botUsername")}
          label={t("telegram.botUsername")}
          hint={t("telegram.botUsernameHint")}
          error={errors.botUsername}
          autoCapitalize="none"
          spellCheck={false}
          value={values.botUsername}
          onChange={set("botUsername")}
        />
        <TextField
          ref={register("appShortName")}
          label={t("telegram.appShortName")}
          hint={t("telegram.appShortNameHint")}
          error={errors.appShortName}
          autoCapitalize="none"
          spellCheck={false}
          value={values.appShortName}
          onChange={set("appShortName")}
        />
        <TextField
          ref={register("botToken")}
          label={t("telegram.botToken")}
          hint={
            loaded.tokenSet
              ? t("telegram.botTokenKeep")
              : t("telegram.botTokenHint")
          }
          keepHint
          error={errors.botToken}
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={values.botToken}
          onChange={set("botToken")}
        />
        <div className="form-actions">
          <button
            type="submit"
            className="button primary"
            disabled={save.isPending}
          >
            {save.isPending ? t("form.submitting") : t("telegram.saveSettings")}
          </button>
        </div>
      </form>
    </>
  );
}
