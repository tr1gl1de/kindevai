import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { TextField } from "../../ui/Field";
import { FormAlert, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { useLogin } from "./api";
import { AuthLayout } from "./AuthLayout";

const fields = ["login", "password"] as const;

export function LoginPage() {
  const { t } = useTranslation();
  const login = useLogin();
  const { errors, show, clear, register } = useFieldErrors(fields);
  const [values, setValues] = useState({ login: "", password: "" });
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const missing = Object.fromEntries(fields.filter((f) => !values[f].trim()).map((f) => [f, t("form.required")]));
    if (Object.keys(missing).length) {
      show(missing);
      return;
    }
    try {
      await login.mutateAsync({ login: values.login.trim(), password: values.password });
    } catch (err) {
      // One message for every credential problem: the server never says which part was wrong.
      setFormError(isApiError(err, "invalid_credentials") ? t("login.invalid") : errorMessage(t, err));
    }
  }

  return (
    <AuthLayout>
      <PageHeading>{t("login.title")}</PageHeading>
      <FormAlert message={formError} />
      <form noValidate onSubmit={onSubmit} aria-label={t("login.title")}>
        <TextField
          ref={register("login")}
          label={t("login.login")}
          error={errors.login}
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          value={values.login}
          onChange={(e) => {
            setValues((v) => ({ ...v, login: e.target.value }));
            clear("login");
          }}
        />
        <TextField
          ref={register("password")}
          label={t("login.password")}
          error={errors.password}
          type="password"
          autoComplete="current-password"
          value={values.password}
          onChange={(e) => {
            setValues((v) => ({ ...v, password: e.target.value }));
            clear("password");
          }}
        />
        <button type="submit" className="button primary" disabled={login.isPending}>
          {login.isPending ? t("form.submitting") : t("login.submit")}
        </button>
      </form>
    </AuthLayout>
  );
}
