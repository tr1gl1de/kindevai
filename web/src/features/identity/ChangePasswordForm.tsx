import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import { TextField } from "../../ui/Field";
import { FormAlert } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useFieldErrors, type FieldErrors } from "../../ui/form";
import { useChangePassword } from "./accountsApi";

const fields = ["current", "next", "repeat"] as const;
type F = (typeof fields)[number];

/** Current + new + repeat password form, shared by the forced first-sign-in screen and the account page. */
export function ChangePasswordForm({ temporary, onDone }: { temporary: boolean; onDone: () => void }) {
  const { t } = useTranslation();
  const change = useChangePassword();
  const { errors, show, clear, register } = useFieldErrors(fields);
  const [values, setValues] = useState<Record<F, string>>({ current: "", next: "", repeat: "" });
  const [formError, setFormError] = useState<string>();
  const set = (f: F) => (e: { target: { value: string } }) => {
    setValues((v) => ({ ...v, [f]: e.target.value }));
    clear(f);
  };
  const fieldErrors = (e: FieldErrors<F>) => {
    show(e);
    setFormError(t("form.summary", { count: Object.keys(e).length }));
  };

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const e: FieldErrors<F> = {};
    if (!values.current) e.current = t("form.required");
    const n = [...values.next].length;
    if (n < 12) e.next = t("validation.password_too_short");
    else if (n > 256) e.next = t("validation.password_too_long");
    else if (values.next === values.current) e.next = t("validation.password_reused");
    if (values.repeat !== values.next) e.repeat = t("validation.password_mismatch");
    if (Object.keys(e).length) {
      fieldErrors(e);
      return;
    }
    try {
      await change.mutateAsync({ currentPassword: values.current, newPassword: values.next });
      onDone();
    } catch (err) {
      if (isApiError(err, "invalid_credentials")) fieldErrors({ current: t("account.wrongCurrent") });
      else if (isApiError(err, "validation") && err.field === "newPassword")
        fieldErrors({ next: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <>
      <FormAlert message={formError} />
      <form noValidate onSubmit={onSubmit} aria-label={t("account.title")}>
        <TextField
          ref={register("current")}
          label={temporary ? t("account.currentTemporary") : t("account.current")}
          error={errors.current}
          type="password"
          autoComplete="current-password"
          value={values.current}
          onChange={set("current")}
        />
        <TextField
          ref={register("next")}
          label={t("account.new")}
          hint={t("account.newHint")}
          error={errors.next}
          type="password"
          autoComplete="new-password"
          value={values.next}
          onChange={set("next")}
        />
        <TextField
          ref={register("repeat")}
          label={t("account.repeat")}
          error={errors.repeat}
          type="password"
          autoComplete="new-password"
          value={values.repeat}
          onChange={set("repeat")}
        />
        <div className="form-actions">
          <button type="submit" className="button primary" disabled={change.isPending}>
            {change.isPending ? t("form.submitting") : t("account.submit")}
          </button>
        </div>
      </form>
    </>
  );
}
