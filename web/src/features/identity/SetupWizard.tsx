import { useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router";
import { isApiError } from "../../api/client";
import { TextField } from "../../ui/Field";
import { FormAlert, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useFieldErrors, type FieldErrors } from "../../ui/form";
import { keys, useCompleteSetup, useLogin } from "./api";
import { AuthLayout } from "./AuthLayout";

const fields = ["token", "login", "password", "passwordRepeat", "platformName"] as const;
type F = (typeof fields)[number];

const loginRe = /^[a-z0-9][a-z0-9._-]{2,63}$/;

export function SetupWizard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const setup = useCompleteSetup();
  const login = useLogin();
  const { errors, show, clear, register, count } = useFieldErrors(fields);
  const [values, setValues] = useState<Record<F, string>>({ token: "", login: "", password: "", passwordRepeat: "", platformName: "" });
  const [formError, setFormError] = useState<string>();
  const busy = setup.isPending || login.isPending;

  const set = (f: F) => (e: { target: { value: string } }) => {
    setValues((v) => ({ ...v, [f]: e.target.value }));
    clear(f);
  };

  function validate(): FieldErrors<F> {
    const e: FieldErrors<F> = {};
    const lg = values.login.trim().toLowerCase();
    if (!values.token.trim()) e.token = t("form.required");
    if (!loginRe.test(lg)) e.login = t("validation.login_format");
    const pwLen = [...values.password].length;
    if (pwLen < 12) e.password = t("validation.password_too_short");
    else if (pwLen > 256) e.password = t("validation.password_too_long");
    else if (values.password.toLowerCase() === lg) e.password = t("validation.password_equals_login");
    if (values.password !== values.passwordRepeat) e.passwordRepeat = t("validation.password_mismatch");
    const name = values.platformName.trim();
    if (!name || [...name].length > 80) e.platformName = t("validation.platform_name_format");
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
    const creds = { login: values.login.trim(), password: values.password };
    try {
      await setup.mutateAsync({ token: values.token.trim(), ...creds, platformName: values.platformName.trim() });
    } catch (err) {
      if (isApiError(err, "invalid_setup_token")) {
        show({ token: t("setup.invalidToken") });
        setFormError(t("setup.invalidToken"));
      } else if (isApiError(err, "setup_completed")) {
        setFormError(t("setup.completed"));
        await qc.invalidateQueries({ queryKey: keys.setup });
      } else if (isApiError(err, "validation") && err.field && fields.includes(err.field as F)) {
        show({ [err.field]: validationMessage(t, err.detail) ?? t("errors.unknown") });
      } else {
        setFormError(errorMessage(t, err));
      }
      return;
    }
    // Owner created: sign in with the same credentials so the operator lands in the dashboard.
    try {
      await login.mutateAsync(creds);
    } finally {
      await qc.invalidateQueries({ queryKey: keys.setup });
    }
    navigate("/projects");
  }

  const summary = formError ?? (count ? t("form.summary", { count }) : undefined);
  return (
    <AuthLayout>
      <PageHeading>{t("setup.title")}</PageHeading>
      <p>{t("setup.intro")}</p>
      <FormAlert message={summary} />
      <form noValidate onSubmit={onSubmit} aria-label={t("setup.title")}>
        <TextField
          ref={register("token")}
          label={t("setup.token")}
          hint={t("setup.tokenHint")}
          error={errors.token}
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={values.token}
          onChange={set("token")}
        />
        <TextField
          ref={register("login")}
          label={t("setup.login")}
          hint={t("setup.loginHint")}
          error={errors.login}
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          value={values.login}
          onChange={set("login")}
        />
        <TextField
          ref={register("password")}
          label={t("setup.password")}
          hint={t("setup.passwordHint")}
          error={errors.password}
          type="password"
          autoComplete="new-password"
          value={values.password}
          onChange={set("password")}
        />
        <TextField
          ref={register("passwordRepeat")}
          label={t("setup.passwordRepeat")}
          error={errors.passwordRepeat}
          type="password"
          autoComplete="new-password"
          value={values.passwordRepeat}
          onChange={set("passwordRepeat")}
        />
        <TextField
          ref={register("platformName")}
          label={t("setup.platformName")}
          hint={t("setup.platformNameHint")}
          error={errors.platformName}
          autoComplete="organization"
          value={values.platformName}
          onChange={set("platformName")}
        />
        <button type="submit" className="button primary" disabled={busy}>
          {busy ? t("form.submitting") : t("setup.submit")}
        </button>
      </form>
    </AuthLayout>
  );
}
