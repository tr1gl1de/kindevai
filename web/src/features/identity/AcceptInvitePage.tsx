import { useEffect, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useLocation, useNavigate } from "react-router";
import { isApiError } from "../../api/client";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useFieldErrors } from "../../ui/form";
import { AuthLayout } from "./AuthLayout";
import { useAcceptInvitation, useInvitationPreview } from "./invitationsApi";

const fields = ["login", "password", "passwordRepeat"] as const;

/** Reads the token from the URL fragment once and removes it from the address bar and history. */
function useFragmentToken(): string {
  const location = useLocation();
  const navigate = useNavigate();
  const [token] = useState(() => new URLSearchParams(location.hash.slice(1)).get("token") ?? "");
  useEffect(() => {
    if (location.hash) navigate(location.pathname, { replace: true });
  }, [location.hash, location.pathname, navigate]);
  return token;
}

/** Public page behind an invitation link: shows the offer, then lets the holder create their own account. */
export function AcceptInvitePage() {
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const token = useFragmentToken();
  const preview = useInvitationPreview(token);
  const accept = useAcceptInvitation();
  const { errors, show, clear, register } = useFieldErrors(fields);
  const [values, setValues] = useState({ login: "", password: "", passwordRepeat: "" });
  const [formError, setFormError] = useState<string>();
  const [created, setCreated] = useState<string | null>(null);
  const doneHeading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (created) doneHeading.current?.focus();
  }, [created]);

  function validate() {
    const e: Partial<Record<(typeof fields)[number], string>> = {};
    const lg = values.login.trim().toLowerCase();
    if (!/^[a-z0-9][a-z0-9._-]{2,63}$/.test(lg)) e.login = t("validation.login_format");
    const pwLen = [...values.password].length;
    if (pwLen < 12) e.password = t("validation.password_too_short");
    else if (pwLen > 256) e.password = t("validation.password_too_long");
    else if (values.password.toLowerCase() === lg) e.password = t("validation.password_equals_login");
    if (values.password !== values.passwordRepeat) e.passwordRepeat = t("validation.password_mismatch");
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
      const r = await accept.mutateAsync({ token, login: values.login.trim(), password: values.password });
      setCreated(r.user.login);
    } catch (err) {
      if (isApiError(err, "login_taken")) show({ login: t("users.loginTaken") });
      else if (isApiError(err, "validation") && (err.field === "login" || err.field === "password"))
        show({ [err.field]: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else setFormError(errorMessage(t, err));
    }
  }

  const set = (f: (typeof fields)[number]) => (e: { target: { value: string } }) => {
    setValues((v) => ({ ...v, [f]: e.target.value }));
    clear(f);
  };

  return (
    <AuthLayout>
      <PageHeading>{t("invite.title")}</PageHeading>
      {!token && <ErrorState message={t("invite.invitation_invalid")} />}
      {token && preview.isPending && <Loading />}
      {token && preview.isError && <ErrorState message={errorMessage(t, preview.error)} />}
      {created && (
        <section className="card secret" aria-labelledby="invite-done">
          <h2 id="invite-done" ref={doneHeading} tabIndex={-1}>
            {t("invite.doneTitle")}
          </h2>
          <p>{t("invite.doneText", { login: created })}</p>
          <div className="form-actions">
            <button type="button" className="button primary" onClick={() => navigate("/", { replace: true })}>
              {t("invite.toSignIn")}
            </button>
          </div>
        </section>
      )}
      {preview.data && !created && (
        <>
          <p>
            {t("invite.offer", {
              project: preview.data.projectName,
              role: t(`projects.role_${preview.data.role}`),
              date: formatDateTime(preview.data.expiresAt, i18n.language),
            })}
          </p>
          <FormAlert message={formError} />
          <form noValidate onSubmit={onSubmit} aria-label={t("invite.title")}>
            <TextField
              ref={register("login")}
              label={t("users.login")}
              hint={t("users.loginHint")}
              error={errors.login}
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={values.login}
              onChange={set("login")}
            />
            <TextField
              ref={register("password")}
              label={t("login.password")}
              hint={t("invite.passwordHint")}
              error={errors.password}
              type="password"
              autoComplete="new-password"
              value={values.password}
              onChange={set("password")}
            />
            <TextField
              ref={register("passwordRepeat")}
              label={t("invite.passwordRepeat")}
              error={errors.passwordRepeat}
              type="password"
              autoComplete="new-password"
              value={values.passwordRepeat}
              onChange={set("passwordRepeat")}
            />
            <div className="form-actions">
              <button type="submit" className="button primary" disabled={accept.isPending}>
                {accept.isPending ? t("form.submitting") : t("invite.submit")}
              </button>
            </div>
          </form>
        </>
      )}
    </AuthLayout>
  );
}
