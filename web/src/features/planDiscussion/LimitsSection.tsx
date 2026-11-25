import { useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { isApiError } from "../../api/client";
import type { DiscussionLimits } from "../../api/types";
import { TextField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useNotify } from "../../ui/notify";
import { useDiscussionLimits, useSaveDiscussionLimits } from "./api";

type Key = "maxMessageChars" | "maxMessagesPerPlan" | "maxMessagesPerHour" | "answerTimeoutMin";

/** Ranges the server enforces (KIN-43 DiscussionLimits.Validate). */
const ranges: Record<Key, [number, number]> = {
  maxMessageChars: [1, 8000],
  maxMessagesPerPlan: [1, 1000],
  maxMessagesPerHour: [1, 1000],
  answerTimeoutMin: [1, 60],
};
const order: Key[] = ["maxMessageChars", "maxMessagesPerPlan", "maxMessagesPerHour", "answerTimeoutMin"];
/** The server names the timeout field answerTimeout. */
const serverField = (f: string | undefined): Key | undefined =>
  f === "answerTimeout" ? "answerTimeoutMin" : order.find((k) => k === f);

/** The Owner's limits of the plan discussion with the AI (KIN-43/44), on the Agents page. */
export function LimitsSection() {
  const { t } = useTranslation();
  const q = useDiscussionLimits();
  return (
    <section aria-labelledby="discussion-limits-title" className="page-section">
      <h2 id="discussion-limits-title">{t("planDiscussion.limitsTitle")}</h2>
      <p className="muted">{t("planDiscussion.limitsIntro")}</p>
      {q.isPending ? (
        <Loading />
      ) : q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : (
        <LimitsForm limits={q.data} />
      )}
    </section>
  );
}

function LimitsForm({ limits }: { limits: DiscussionLimits }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveDiscussionLimits();
  const [values, setValues] = useState<Record<Key, string>>({
    maxMessageChars: String(limits.maxMessageChars),
    maxMessagesPerPlan: String(limits.maxMessagesPerPlan),
    maxMessagesPerHour: String(limits.maxMessagesPerHour),
    answerTimeoutMin: String(Math.round(limits.answerTimeoutSec / 60)),
  });
  const [submitted, setSubmitted] = useState(false);
  const [serverErrors, setServerErrors] = useState<Partial<Record<Key, string>>>({});
  const [error, setError] = useState<string>();
  const refs = useRef<Partial<Record<Key, HTMLInputElement | null>>>({});

  const check = (k: Key, v: string) => {
    const [lo, hi] = ranges[k];
    const n = Number(v);
    return /^\d+$/.test(v.trim()) && n >= lo && n <= hi ? undefined : t("planDiscussion.rangeError", { min: lo, max: hi });
  };
  const errors = Object.fromEntries(
    order.map((k) => [k, (submitted ? check(k, values[k]) : undefined) ?? serverErrors[k]]),
  ) as Partial<Record<Key, string>>;
  const invalid = order.filter((k) => errors[k]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    setServerErrors({});
    setError(undefined);
    const bad = order.find((k) => check(k, values[k]));
    if (bad) {
      refs.current[bad]?.focus();
      return;
    }
    save.mutate(
      {
        maxMessageChars: Number(values.maxMessageChars),
        maxMessagesPerPlan: Number(values.maxMessagesPerPlan),
        maxMessagesPerHour: Number(values.maxMessagesPerHour),
        answerTimeoutSec: Number(values.answerTimeoutMin) * 60,
      },
      {
        onSuccess: () => notify(t("planDiscussion.limitsSaved")),
        onError: (err) => {
          const k = isApiError(err, "validation") ? serverField(err.field) : undefined;
          if (k) {
            const [lo, hi] = ranges[k];
            setServerErrors({ [k]: t("planDiscussion.rangeError", { min: lo, max: hi }) });
            refs.current[k]?.focus();
            return;
          }
          setError(errorMessage(t, err));
        },
      },
    );
  };

  return (
    <form noValidate className="card form-card" onSubmit={submit}>
      <FormAlert message={invalid.length > 0 ? t("form.summary", { count: invalid.length }) : error} />
      {order.map((k) => (
        <TextField
          key={k}
          ref={(el) => {
            refs.current[k] = el;
          }}
          label={t(`planDiscussion.limit_${k}`)}
          hint={t(`planDiscussion.limitHint_${k}`)}
          error={errors[k]}
          keepHint
          type="number"
          inputMode="numeric"
          min={ranges[k][0]}
          max={ranges[k][1]}
          value={values[k]}
          onChange={(e) => {
            const v = e.target.value;
            setValues((old) => ({ ...old, [k]: v }));
            setServerErrors((old) => ({ ...old, [k]: undefined }));
          }}
        />
      ))}
      <div>
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("planDiscussion.limitsSave")}
        </button>
      </div>
    </form>
  );
}
