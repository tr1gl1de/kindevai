import { useEffect, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { Account } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { CopyButton } from "../../ui/CopyButton";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { useCreateUser, useResetPassword, useSetDisabled, useUsers } from "./accountsApi";
import { InvitationsSection } from "./InvitationsSection";

type Confirm = { kind: "disable" | "reset"; user: Account } | null;

/** Owner-only account management: create, disable/enable, reset password. */
export function UsersPage() {
  const { t } = useTranslation();
  const notify = useNotify();
  const users = useUsers(true);
  const setDisabled = useSetDisabled();
  const reset = useResetPassword();
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [secret, setSecret] = useState<{ login: string; password: string } | null>(null);
  const [actionError, setActionError] = useState<string>();

  async function run(c: NonNullable<Confirm>) {
    setActionError(undefined);
    try {
      if (c.kind === "disable") {
        await setDisabled.mutateAsync({ id: c.user.id, disabled: true });
        notify(t("users.disabledDone", { login: c.user.login }));
      } else {
        const r = await reset.mutateAsync(c.user.id);
        reset.reset(); // drop the response (with the password) from the mutation
        setSecret({ login: c.user.login, password: r.temporaryPassword });
      }
    } catch (err) {
      setActionError(errorMessage(t, err));
    } finally {
      setConfirm(null);
    }
  }

  async function enable(u: Account) {
    setActionError(undefined);
    try {
      await setDisabled.mutateAsync({ id: u.id, disabled: false });
      notify(t("users.enabledDone", { login: u.login }));
    } catch (err) {
      setActionError(errorMessage(t, err));
    }
  }

  return (
    <>
      <PageHeading>{t("users.title")}</PageHeading>
      {secret && <TemporaryPassword key={secret.password} {...secret} onDone={() => setSecret(null)} />}
      <NewUser blocked={secret !== null} onCreated={(login, password) => setSecret({ login, password })} />
      <FormAlert message={actionError} />
      {users.isPending && <Loading />}
      {users.isError && <ErrorState message={errorMessage(t, users.error)} onRetry={() => void users.refetch()} />}
      {users.data && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">{t("users.login")}</th>
                <th scope="col">{t("users.role")}</th>
                <th scope="col">{t("users.status")}</th>
                <th scope="col">{t("users.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {users.data.map((u) => (
                <tr key={u.id}>
                  <td>{u.login}</td>
                  <td>{t(`users.role_${u.role}`)}</td>
                  <td>{u.disabled ? t("users.disabled") : u.mustChangePassword ? t("users.mustChange") : t("users.active")}</td>
                  <td className="row-actions">
                    {u.role !== "owner" &&
                      (u.disabled ? (
                        <button type="button" className="button subtle" aria-label={t("users.enableLabel", { login: u.login })} onClick={() => void enable(u)}>
                          {t("users.enable")}
                        </button>
                      ) : (
                        <button
                          type="button"
                          className="button subtle"
                          aria-label={t("users.disableLabel", { login: u.login })}
                          onClick={() => setConfirm({ kind: "disable", user: u })}
                        >
                          {t("users.disable")}
                        </button>
                      ))}
                    {u.role !== "owner" && (
                      <button
                        type="button"
                        className="button subtle"
                        aria-label={t("users.resetLabel", { login: u.login })}
                        disabled={secret !== null}
                        onClick={() => setConfirm({ kind: "reset", user: u })}
                      >
                        {t("users.reset")}
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {confirm && (
        <ConfirmDialog
          title={t(confirm.kind === "disable" ? "users.disableTitle" : "users.resetTitle", { login: confirm.user.login })}
          text={t(confirm.kind === "disable" ? "users.disableText" : "users.resetText")}
          confirmLabel={t(confirm.kind === "disable" ? "users.disable" : "users.reset")}
          cancelLabel={t("users.cancel")}
          busy={setDisabled.isPending || reset.isPending}
          onConfirm={() => void run(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
      <InvitationsSection />
    </>
  );
}

function NewUser({ blocked, onCreated }: { blocked: boolean; onCreated: (login: string, password: string) => void }) {
  const { t } = useTranslation();
  const create = useCreateUser();
  const { errors, show, clear, register } = useFieldErrors(["login"] as const);
  const [login, setLogin] = useState("");
  const [formError, setFormError] = useState<string>();

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    if (!/^[a-z0-9][a-z0-9._-]{2,63}$/.test(login.trim().toLowerCase())) {
      show({ login: t("validation.login_format") });
      return;
    }
    try {
      const r = await create.mutateAsync({ login: login.trim() });
      create.reset(); // drop the response (with the password) from the mutation
      onCreated(r.user?.login ?? login.trim(), r.temporaryPassword);
      setLogin("");
    } catch (err) {
      if (isApiError(err, "login_taken")) show({ login: t("users.loginTaken") });
      else if (isApiError(err, "validation")) show({ login: validationMessage(t, err.detail) ?? t("errors.unknown") });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={onSubmit} aria-label={t("users.new")} className="card form-card inline-form">
      <FormAlert message={formError} />
      <TextField
        ref={register("login")}
        label={t("users.login")}
        hint={t("users.loginHint")}
        error={errors.login}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        value={login}
        onChange={(e) => {
          setLogin(e.target.value);
          clear("login");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={create.isPending || blocked}>
          {create.isPending ? t("form.submitting") : t("users.create")}
        </button>
      </div>
    </form>
  );
}

/** The one-time display of a generated password, with copy and an explicit "Done" that removes it. */
function TemporaryPassword({ login, password, onDone }: { login: string; password: string; onDone: () => void }) {
  const { t } = useTranslation();
  const title = t("users.tempTitle", { login });
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), []); // announce and bring the one-time secret into view
  return (
    <section className="card secret" aria-label={title}>
      <h2 ref={heading} tabIndex={-1}>
        {title}
      </h2>
      <p>{t("users.tempText", { login })}</p>
      <p>
        <code className="secret-value">{password}</code>
      </p>
      <div className="form-actions">
        <CopyButton text={password} failedHint={t("users.copyFailed")} />
        <button type="button" className="button primary" onClick={onDone}>
          {t("users.done")}
        </button>
      </div>
    </section>
  );
}
