import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { RiskCategory, RiskLevel, RiskPolicy, RiskRule } from "../../api/types";
import { SelectField, TextAreaField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { useNotify } from "../../ui/notify";
import { useRiskPolicy, useSaveRiskPolicy } from "./api";
import "./approvals.css";

const levels: RiskLevel[] = ["L0", "L1", "L2", "L3"];

/** The project's deterministic risk policy: the Owner edits it, a Project Admin reads it. */
export function RiskPolicyPanel({ projectId, canEdit }: { projectId: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const policy = useRiskPolicy(projectId);
  return (
    <section aria-labelledby="risk-policy-title" className="page-section">
      <h2 id="risk-policy-title">{t("riskPolicy.title")}</h2>
      <p className="muted">{t("riskPolicy.intro")}</p>
      {policy.isPending ? (
        <Loading />
      ) : policy.isError ? (
        <ErrorState message={errorMessage(t, policy.error)} onRetry={() => void policy.refetch()} />
      ) : (
        <>
          <p>{t(policy.data.custom ? "riskPolicy.custom" : "riskPolicy.default")}</p>
          {canEdit ? (
            <PolicyForm key={JSON.stringify(policy.data)} projectId={projectId} policy={policy.data} />
          ) : (
            <ReadOnly policy={policy.data} />
          )}
        </>
      )}
    </section>
  );
}

function ReadOnly({ policy }: { policy: RiskPolicy }) {
  const { t } = useTranslation();
  return (
    <>
      <div className="policy-rules">
        {policy.rules.map((r) => (
          <div key={r.category} className="card">
            <h3>
              {t(`approvals.category_${r.category}`)} · <span className="badge">{r.risk}</span>
            </h3>
            {r.patterns.length ? (
              <ul className="paths">
                {r.patterns.map((p) => (
                  <li key={p}>
                    <code>{p}</code>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted">{t("riskPolicy.none")}</p>
            )}
          </div>
        ))}
      </div>
      <p className="muted">{t("riskPolicy.readOnly")}</p>
    </>
  );
}

interface Draft {
  category: RiskCategory;
  risk: RiskLevel;
  text: string;
}

const toDraft = (r: RiskRule): Draft => ({ category: r.category, risk: r.risk, text: r.patterns.join("\n") });
const toRule = (d: Draft): RiskRule => ({
  category: d.category,
  risk: d.risk,
  patterns: d.text
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean),
});

function PolicyForm({ projectId, policy }: { projectId: string; policy: RiskPolicy }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveRiskPolicy(projectId);
  const [drafts, setDrafts] = useState<Draft[]>(() => policy.rules.map(toDraft));
  const [fieldErrors, setFieldErrors] = useState<Record<number, string>>({});
  const [error, setError] = useState<string>();
  const refs = useRef<(HTMLTextAreaElement | null)[]>([]);

  const update = (i: number, patch: Partial<Draft>) => setDrafts((ds) => ds.map((d, j) => (j === i ? { ...d, ...patch } : d)));

  async function submit(ev: FormEvent) {
    ev.preventDefault();
    setError(undefined);
    setFieldErrors({});
    try {
      await save.mutateAsync(drafts.map(toRule));
      notify(t("riskPolicy.saved"));
    } catch (err) {
      const m = isApiError(err, "validation") ? /^rules\[(\d+)\]/.exec(err.field ?? "") : null;
      if (m && isApiError(err)) {
        const i = Number(m[1]);
        setFieldErrors({ [i]: validationMessage(t, err.detail) ?? t("errors.unknown") });
        setError(t("form.summary", { count: 1 }));
        refs.current[i]?.focus();
        return;
      }
      setError(errorMessage(t, err));
    }
  }

  return (
    <form noValidate onSubmit={(e) => void submit(e)} aria-labelledby="risk-policy-title">
      <FormAlert message={error} />
      <div className="policy-rules">
        {drafts.map((d, i) => {
          const name = t(`approvals.category_${d.category}`);
          return (
            <fieldset key={d.category} className="card">
              <legend>{name}</legend>
              <SelectField
                label={t("riskPolicy.riskLabel", { category: name })}
                value={d.risk}
                onChange={(e) => update(i, { risk: e.target.value as RiskLevel })}
              >
                {levels.map((l) => (
                  <option key={l} value={l}>
                    {l}
                  </option>
                ))}
              </SelectField>
              <TextAreaField
                ref={(el) => {
                  refs.current[i] = el;
                }}
                label={t("riskPolicy.patternsLabel", { category: name })}
                hint={t("riskPolicy.patternsHint")}
                keepHint
                error={fieldErrors[i]}
                rows={4}
                spellCheck={false}
                value={d.text}
                onChange={(e) => update(i, { text: e.target.value })}
              />
            </fieldset>
          );
        })}
      </div>
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("riskPolicy.save")}
        </button>
      </div>
    </form>
  );
}
