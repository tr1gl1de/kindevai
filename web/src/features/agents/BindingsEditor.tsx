import { useState, type FormEvent } from "react";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type {
  AgentBinding,
  AgentChoice,
  AgentCli,
  BindingStrategy,
} from "../../api/types";
import { SelectField, TextField } from "../../ui/Field";
import { FormAlert } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useNotify } from "../../ui/notify";
import { useResetBinding, useSaveBinding, type AgentScope } from "./api";
import "./agents.css";

export const clis: AgentCli[] = ["claude", "codex"];

const bindingErrors = [
  "agent_unknown",
  "model_format",
  "duplicate_choice",
  "too_many_fallbacks",
  "strategy_reviewer_only",
  "strategy_unknown",
] as const;

/** Message for a binding validation code from the server. */
function bindingError(t: TFunction, code: string | undefined): string {
  const known = bindingErrors.find((c) => c === code);
  return known
    ? t(`agents.err_${known}`)
    : (validationMessage(t, code) ?? t("errors.unknown"));
}
const maxFallbacks = 4;

/** Role bindings of a scope: one form per role (primary + ordered fallback, reviewer strategy). */
export function BindingsEditor({
  scope,
  bindings,
}: {
  scope: AgentScope;
  bindings: AgentBinding[];
}) {
  return (
    <div className="bindings">
      {bindings.map((b) => (
        <BindingForm
          key={`${b.role}:${JSON.stringify(b)}`}
          scope={scope}
          binding={b}
        />
      ))}
    </div>
  );
}

function BindingForm({
  scope,
  binding,
}: {
  scope: AgentScope;
  binding: AgentBinding;
}) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveBinding(scope);
  const reset = useResetBinding(scope);
  const [chain, setChain] = useState<AgentChoice[]>(() => [
    binding.primary,
    ...binding.fallback,
  ]);
  const [strategy, setStrategy] = useState<BindingStrategy>(binding.strategy);
  const [error, setError] = useState<string>();
  const [fieldError, setFieldError] = useState<{
    index: number;
    message: string;
  }>();
  const role = t(`agents.role_${binding.role}`);
  const own =
    scope.projectId === undefined
      ? binding.source === "global"
      : binding.source === "project";

  const update = (i: number, patch: Partial<AgentChoice>) =>
    setChain((c) => c.map((x, j) => (j === i ? { ...x, ...patch } : x)));

  async function submit(ev: FormEvent) {
    ev.preventDefault();
    setError(undefined);
    setFieldError(undefined);
    const [primary, ...fallback] = chain;
    try {
      await save.mutateAsync({
        role: binding.role,
        body: { primary: primary!, fallback, strategy },
      });
      notify(t("agents.bindingSaved", { role }));
    } catch (err) {
      if (isApiError(err, "validation")) {
        const m = /^(primary|fallback\[(\d+)\])/.exec(err.field ?? "");
        if (m) {
          const index = m[1] === "primary" ? 0 : Number(m[2]) + 1;
          setFieldError({ index, message: bindingError(t, err.detail) });
          setError(t("form.summary", { count: 1 }));
          return;
        }
      }
      setError(
        isApiError(err, "validation")
          ? bindingError(t, err.detail)
          : errorMessage(t, err),
      );
    }
  }

  async function doReset() {
    setError(undefined);
    try {
      await reset.mutateAsync(binding.role);
      notify(t("agents.bindingReset", { role }));
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  return (
    <form
      noValidate
      className="card binding-form"
      onSubmit={(e) => void submit(e)}
      aria-labelledby={`binding-${binding.role}`}
    >
      <h3 id={`binding-${binding.role}`}>{role}</h3>
      <p className="muted">{t(`agents.source_${binding.source}`)}</p>
      <FormAlert message={error} />
      {chain.map((c, i) => (
        <fieldset key={i} className="choice">
          <legend>
            {i === 0 ? t("agents.primary") : t("agents.fallbackN", { n: i })}
          </legend>
          <SelectField
            label={t("agents.cliLabel")}
            value={c.agent}
            error={fieldError?.index === i ? fieldError.message : undefined}
            onChange={(e) => update(i, { agent: e.target.value as AgentCli })}
          >
            {clis.map((a) => (
              <option key={a} value={a}>
                {t(`agents.cli_${a}`)}
              </option>
            ))}
          </SelectField>
          <TextField
            label={t("agents.modelLabel")}
            hint={t("agents.modelHint")}
            value={c.model}
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => update(i, { model: e.target.value })}
          />
          {i > 0 && (
            <button
              type="button"
              className="button subtle"
              onClick={() => setChain((x) => x.filter((_, j) => j !== i))}
            >
              {t("agents.removeFallback", { n: i })}
            </button>
          )}
        </fieldset>
      ))}
      {chain.length <= maxFallbacks && (
        <button
          type="button"
          className="button subtle"
          onClick={() => setChain((x) => [...x, { agent: "codex", model: "" }])}
        >
          {t("agents.addFallback")}
        </button>
      )}
      {binding.role === "reviewer" && (
        <SelectField
          label={t("agents.strategyLabel")}
          hint={t("agents.strategyHint")}
          value={strategy}
          onChange={(e) => setStrategy(e.target.value as BindingStrategy)}
        >
          <option value="different-from-author">
            {t("agents.strategy_different")}
          </option>
          <option value="ordered">{t("agents.strategy_ordered")}</option>
        </SelectField>
      )}
      <div className="form-actions">
        <button
          type="submit"
          className="button primary"
          disabled={save.isPending}
        >
          {save.isPending
            ? t("form.submitting")
            : t("agents.saveBinding", { role })}
        </button>
        {own && (
          <button
            type="button"
            className="button subtle"
            disabled={reset.isPending}
            onClick={() => void doReset()}
          >
            {scope.projectId === undefined
              ? t("agents.resetDefault")
              : t("agents.resetInherit")}
          </button>
        )}
      </div>
    </form>
  );
}
