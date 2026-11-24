import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { CopyButton } from "../../ui/CopyButton";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { fieldError } from "./IntegrationsPage";
import {
  useDisableWebhook,
  useIntakeSettings,
  useLinkYouTrackUser,
  useSavePollInterval,
  useSaveWebhookSecret,
  useUnlinkYouTrackUser,
  type IntakeSettings,
  type IntakeState,
  type YouTrackUserLink,
} from "./intakeApi";

/** Owner-only: how YouTrack issues come in — polling, the optional webhook and the YouTrack ↔ account links. */
export function IntakeSettingsSection() {
  const { t } = useTranslation();
  const settings = useIntakeSettings();
  return (
    <section aria-labelledby="intake-title" className="section">
      <h2 id="intake-title">{t("intake.title")}</h2>
      <p className="muted">{t("intake.intro")}</p>
      {settings.isPending && <Loading />}
      {settings.isError && <ErrorState message={errorMessage(t, settings.error)} onRetry={() => void settings.refetch()} />}
      {settings.isSuccess && (
        <>
          <Health health={settings.data.health} />
          <PollInterval key={settings.data.pollIntervalSeconds} current={settings.data.pollIntervalSeconds} />
          <Webhook settings={settings.data} />
          <UserLinks links={settings.data.links} />
        </>
      )}
    </section>
  );
}

export function StateText({ state }: { state: IntakeState }) {
  const { t } = useTranslation();
  const known: IntakeState[] = ["ok", "degraded", "circuit_open"];
  return <strong>{t(`intake.state_${known.includes(state) ? state : "degraded"}`)}</strong>;
}

function Health({ health }: { health: IntakeSettings["health"] }) {
  const { t } = useTranslation();
  return (
    <p aria-live="polite">
      {t("intake.statusTitle")}: <StateText state={health.state} />
      {health.lastError && health.state !== "ok" && <span className="muted"> {t("intake.lastError", { error: health.lastError })}</span>}
    </p>
  );
}

function PollInterval({ current }: { current: number }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSavePollInterval();
  const { errors, show, clear, register } = useFieldErrors(["pollIntervalSeconds"] as const);
  const [value, setValue] = useState(String(current));
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const n = Number(value);
    if (!Number.isInteger(n) || n < 15 || n > 3600) {
      show({ pollIntervalSeconds: t("validation.poll_interval_range") });
      return;
    }
    try {
      await save.mutateAsync(n);
      notify(t("intake.pollSaved"));
    } catch (err) {
      if (isApiError(err, "validation")) show({ pollIntervalSeconds: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("intake.pollTitle")} className="card form-card inline-form">
      <h3>{t("intake.pollTitle")}</h3>
      <FormAlert message={formError} />
      <TextField
        ref={register("pollIntervalSeconds")}
        label={t("intake.pollInterval")}
        hint={t("intake.pollIntervalHint")}
        error={errors.pollIntervalSeconds}
        type="number"
        inputMode="numeric"
        min={15}
        max={3600}
        value={value}
        onChange={(e) => {
          setValue(e.target.value);
          clear("pollIntervalSeconds");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("intake.savePoll")}
        </button>
      </div>
    </form>
  );
}

/** 48 URL-safe random characters from the browser's CSPRNG. */
function generateSecret(): string {
  const bytes = new Uint8Array(36);
  crypto.getRandomValues(bytes);
  return btoa(String.fromCharCode(...bytes)).replaceAll("+", "-").replaceAll("/", "_");
}

function Webhook({ settings }: { settings: IntakeSettings }) {
  const { t, i18n } = useTranslation();
  const notify = useNotify();
  const save = useSaveWebhookSecret();
  const disable = useDisableWebhook();
  const { errors, show, clear, register } = useFieldErrors(["webhookSecret"] as const);
  const [secret, setSecret] = useState("");
  const [generated, setGenerated] = useState(false);
  const [formError, setFormError] = useState<string>();
  const [confirm, setConfirm] = useState(false);

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    if (!secret.trim()) {
      show({ webhookSecret: t("form.required") });
      return;
    }
    try {
      await save.mutateAsync(secret.trim());
      save.reset();
      setSecret("");
      setGenerated(false);
      notify(t("intake.webhookSaved"));
    } catch (err) {
      save.reset();
      if (isApiError(err, "validation")) show({ webhookSecret: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  async function doDisable() {
    setFormError(undefined);
    try {
      await disable.mutateAsync();
      notify(t("intake.webhookDisabled"));
    } catch (err) {
      setFormError(errorMessage(t, err));
    } finally {
      setConfirm(false);
    }
  }

  const ws = settings.webhookSecret;
  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("intake.webhookTitle")} className="card form-card">
      <h3>{t("intake.webhookTitle")}</h3>
      <p className="muted">{t("intake.webhookHint", { path: settings.webhookPath, header: settings.webhookHeader })}</p>
      <p>
        {ws.set
          ? t("intake.webhookSecretSet", { date: ws.updatedAt ? formatDateTime(ws.updatedAt, i18n.language) : "" })
          : t("intake.webhookOff")}
      </p>
      <FormAlert message={formError} />
      <TextField
        ref={register("webhookSecret")}
        label={t("intake.webhookSecret")}
        hint={generated ? t("intake.generatedSecret") : t("intake.webhookSecretHint")}
        keepHint
        error={errors.webhookSecret}
        type={generated ? "text" : "password"}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        value={secret}
        onChange={(e) => {
          setSecret(e.target.value);
          setGenerated(false);
          clear("webhookSecret");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("intake.saveWebhook")}
        </button>
        <button
          type="button"
          className="button"
          onClick={() => {
            setSecret(generateSecret());
            setGenerated(true);
            clear("webhookSecret");
          }}
        >
          {t("intake.generateSecret")}
        </button>
        {generated && secret && <CopyButton text={secret} failedHint={t("intake.generatedSecret")} />}
        {ws.set && (
          <button type="button" className="button subtle" onClick={() => setConfirm(true)}>
            {t("intake.disableWebhook")}
          </button>
        )}
      </div>
      {confirm && (
        <ConfirmDialog
          title={t("intake.disableWebhook")}
          text={t("intake.webhookOff")}
          confirmLabel={t("intake.disableWebhook")}
          cancelLabel={t("users.cancel")}
          busy={disable.isPending}
          onConfirm={() => void doDisable()}
          onCancel={() => setConfirm(false)}
        />
      )}
    </form>
  );
}

const linkFields = ["login", "youtrackLogin"] as const;

function UserLinks({ links }: { links: YouTrackUserLink[] }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const link = useLinkYouTrackUser();
  const unlink = useUnlinkYouTrackUser();
  const { errors, show, clear, register, count } = useFieldErrors(linkFields);
  const [values, setValues] = useState({ login: "", youtrackLogin: "" });
  const [formError, setFormError] = useState<string>();
  const [listError, setListError] = useState<string>();
  const [confirm, setConfirm] = useState<YouTrackUserLink | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const missing: Partial<Record<(typeof linkFields)[number], string>> = {};
    if (!values.login.trim()) missing.login = t("form.required");
    if (!values.youtrackLogin.trim()) missing.youtrackLogin = t("form.required");
    if (Object.keys(missing).length) {
      show(missing);
      return;
    }
    try {
      const l = await link.mutateAsync({ login: values.login.trim(), youtrackLogin: values.youtrackLogin.trim() });
      notify(t("intake.linked", { login: l.login, youtrackLogin: l.youtrackLogin }));
      setValues({ login: "", youtrackLogin: "" });
    } catch (err) {
      const f = isApiError(err) ? err.field : undefined;
      if (isApiError(err, "youtrack_login_taken")) show({ youtrackLogin: t("intake.loginTaken") });
      else if (isApiError(err, "validation") && (f === "login" || f === "youtrackLogin")) show({ [f]: fieldError(t, err.detail) });
      else setFormError(errorMessage(t, err));
    }
  }

  async function doUnlink(l: YouTrackUserLink) {
    setListError(undefined);
    try {
      await unlink.mutateAsync(l.userId);
      notify(t("intake.unlinked", { login: l.login || l.youtrackLogin }));
    } catch (err) {
      setListError(errorMessage(t, err));
    } finally {
      setConfirm(null);
      queueMicrotask(() => heading.current?.focus());
    }
  }

  const field = (k: (typeof linkFields)[number]) => ({
    ref: register(k),
    error: errors[k],
    autoCapitalize: "none",
    autoComplete: "off",
    spellCheck: false,
    value: values[k],
    onChange: (e: React.ChangeEvent<HTMLInputElement>) => {
      setValues((v) => ({ ...v, [k]: e.target.value }));
      clear(k);
    },
  });

  return (
    <>
      <h3 ref={heading} tabIndex={-1}>
        {t("intake.usersTitle")}
      </h3>
      <p className="muted">{t("intake.usersHint")}</p>
      <FormAlert message={listError} />
      {links.length === 0 ? (
        <p className="muted">{t("intake.noLinks")}</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("intake.account")}</th>
                <th scope="col">{t("intake.youtrackLogin")}</th>
                <th scope="col">
                  <span className="visually-hidden">{t("members.actions")}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {links.map((l) => (
                <tr key={l.userId}>
                  <td>{l.login}</td>
                  <td>{l.youtrackLogin}</td>
                  <td className="row-actions">
                    <button
                      type="button"
                      className="button subtle"
                      aria-label={t("intake.unlinkLabel", { login: l.login || l.youtrackLogin })}
                      onClick={() => setConfirm(l)}
                    >
                      {t("intake.unlink")}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {confirm && (
        <ConfirmDialog
          title={t("intake.unlinkTitle", { login: confirm.login || confirm.youtrackLogin, youtrackLogin: confirm.youtrackLogin })}
          text={t("intake.unlinkText")}
          confirmLabel={t("intake.unlink")}
          cancelLabel={t("users.cancel")}
          busy={unlink.isPending}
          onConfirm={() => void doUnlink(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
      <form noValidate onSubmit={onSubmit} aria-label={t("intake.linkUser")} className="card form-card inline-form">
        <FormAlert message={count ? t("form.summary", { count }) : formError} />
        <TextField label={t("intake.account")} {...field("login")} />
        <TextField label={t("intake.youtrackLogin")} {...field("youtrackLogin")} />
        <div className="form-actions">
          <button type="submit" className="button primary" disabled={link.isPending}>
            {link.isPending ? t("form.submitting") : t("intake.linkUser")}
          </button>
        </div>
      </form>
    </>
  );
}
