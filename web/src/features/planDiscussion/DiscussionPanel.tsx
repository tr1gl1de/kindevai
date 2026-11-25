import type { TFunction } from "i18next";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router";
import type { Approval, DiscussionMessage, DiscussionThread } from "../../api/types";
import { TextAreaField } from "../../ui/Field";
import { ErrorState, FormAlert, Loading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { useMe } from "../identity/api";
import { isOpen, limitHit, useDiscussion, usePostMessage, useRetryMessage, type LimitHit } from "./api";
import { RichText } from "./RichText";
import "./planDiscussion.css";

export interface DiscussionPanelProps {
  taskId: string;
  /** The task's current plan revision. */
  currentRevision: number;
  /** The viewer may manage the task (Owner / project admin); undefined when the page does not know. */
  canManage?: boolean;
  /** The task's approvals, to link a new revision to its approval (which shows the diff); omitted when not visible. */
  approvals?: Approval[];
  /** h3 inside a page section (task page), h2 as a page section of its own (approval page). */
  level?: 2 | 3;
}

/**
 * The plan discussion with the AI (KIN-44 over the KIN-43 API): the thread, the state of each message, revisions the
 * answers created and a composer for the Owner and project admins. Message texts are untrusted and shown as text.
 */
export function DiscussionPanel({ taskId, currentRevision, canManage, approvals, level = 3 }: DiscussionPanelProps) {
  const { t } = useTranslation();
  const q = useDiscussion(taskId);
  const titleId = useId();
  const H = level === 2 ? "h2" : "h3";
  useRefreshOnNews(taskId, q.data);

  return (
    <section aria-labelledby={titleId} className={`discussion ${level === 2 ? "page-section" : "card"}`}>
      <H id={titleId}>{t("planDiscussion.title")}</H>
      <p className="muted">{t("planDiscussion.intro")}</p>
      {q.isPending ? (
        <Loading label={t("planDiscussion.loading")} />
      ) : q.isError ? (
        <ErrorState message={errorMessage(t, q.error)} onRetry={() => void q.refetch()} />
      ) : (
        <Thread level={level} taskId={taskId} th={q.data} currentRevision={currentRevision} canManage={canManage} approvals={approvals} />
      )}
    </section>
  );
}

/** A new answer or notice may have changed the plan: refresh the task and its approvals. */
function useRefreshOnNews(taskId: string, th: DiscussionThread | undefined) {
  const qc = useQueryClient();
  const last = th?.messages.at(-1);
  const mark = last ? `${last.id}|${last.status}|${th?.messages.length ?? 0}` : "";
  const seen = useRef<string>(undefined);
  useEffect(() => {
    if (!mark) return;
    if (seen.current !== undefined && seen.current !== mark) {
      void qc.invalidateQueries({ queryKey: ["tasks", "one", taskId] });
      void qc.invalidateQueries({ queryKey: ["approvals"] });
    }
    seen.current = mark;
  }, [mark, qc, taskId]);
}

function Thread({ level, taskId, th, currentRevision, canManage, approvals }: {
  level: 2 | 3;
  taskId: string;
  th: DiscussionThread;
  currentRevision: number;
  canManage?: boolean;
  approvals?: Approval[];
}) {
  const { t } = useTranslation();
  const me = useMe(true);
  const lastFailed = [...th.messages].reverse().find((m) => m.role === "user");
  const retryable = th.canWrite && lastFailed?.status === "FAILED" ? lastFailed.id : undefined;
  const logLabel = t("planDiscussion.messages");
  return (
    <>
      {th.messages.length === 0 ? (
        <p className="discussion-empty">{t("planDiscussion.empty")}</p>
      ) : (
        <div role="log" aria-label={logLabel} className="discussion-log">
          <ol className="discussion-list">
            {th.messages.map((m) => (
              <MessageItem key={m.id} level={level} m={m} mine={m.author === me.data?.id} taskId={taskId} retryable={m.id === retryable}
                currentRevision={currentRevision} approvals={approvals} />
            ))}
          </ol>
        </div>
      )}
      <p className="muted">{t("planDiscussion.used", { used: th.used, max: th.limits.maxMessagesPerPlan })}</p>
      {th.inFlight && (
        <p className="discussion-busy" aria-busy="true">
          <span className="spinner" aria-hidden="true" /> {t("planDiscussion.inFlight")}
        </p>
      )}
      {th.canWrite ? (
        <Composer taskId={taskId} th={th} />
      ) : (
        <p className="card discussion-readonly">
          {canManage === false ? t("planDiscussion.readOnly") : canManage ? t("planDiscussion.closed") : t("planDiscussion.cannotWrite")}
        </p>
      )}
    </>
  );
}

function MessageItem({ level, m, mine, taskId, retryable, currentRevision, approvals }: {
  level: 2 | 3;
  m: DiscussionMessage;
  mine: boolean;
  taskId: string;
  retryable: boolean;
  currentRevision: number;
  approvals?: Approval[];
}) {
  const { t, i18n } = useTranslation();
  if (m.role === "system") {
    if (m.notice === "revision_created" && m.revision) {
      return (
        <li className="discussion-msg discussion-system">
          <RevisionCard level={level} taskId={taskId} revision={m.revision} currentRevision={currentRevision} approvals={approvals}
            at={formatDateTime(m.createdAt, i18n.language)} />
        </li>
      );
    }
    return (
      <li className="discussion-msg discussion-system">
        <p className="card warn">{noticeText(t, m)}</p>
      </li>
    );
  }
  const author =
    m.role === "assistant"
      ? [t("planDiscussion.authorAI"), m.agent, m.model].filter(Boolean).join(" · ")
      : mine
        ? t("planDiscussion.authorYou")
        : t("planDiscussion.authorOther");
  return (
    <li className={`discussion-msg discussion-${m.role}`}>
      <div className="discussion-meta">
        <strong>{author}</strong>
        <span className="muted">{formatDateTime(m.createdAt, i18n.language)}</span>
        <span className="badge">{t("planDiscussion.aboutRevision", { n: m.planRevision })}</span>
        {m.role === "user" && (
          <span className={`badge discussion-status status-${m.status}`}>
            {isOpen(m) && <span className="spinner" aria-hidden="true" />}
            {t(`planDiscussion.status.${m.status}`)}
          </span>
        )}
      </div>
      {m.role === "assistant" ? <RichText text={m.text} /> : <p className="discussion-text">{m.text}</p>}
      {m.status === "FAILED" && m.reason && <p className="field-error">{m.reason}</p>}
      {retryable && <RetryButton taskId={taskId} id={m.id} />}
    </li>
  );
}

const rejectedPrefix = "the proposed plan was not accepted: ";

function noticeText(t: TFunction, m: DiscussionMessage): string {
  switch (m.notice) {
    case "plan_rejected":
      return t("planDiscussion.notices.plan_rejected", {
        reason: m.text.startsWith(rejectedPrefix) ? m.text.slice(rejectedPrefix.length) : m.text,
      });
    case "plan_outdated":
    case "plan_not_applied":
      return t(`planDiscussion.notices.${m.notice}`);
    default:
      return m.text;
  }
}

/** The revision an answer created: what it means for the approval and where its diff is. */
function RevisionCard({ level, taskId, revision, currentRevision, approvals, at }: {
  level: 2 | 3;
  taskId: string;
  revision: number;
  currentRevision: number;
  approvals?: Approval[];
  at: string;
}) {
  const { t } = useTranslation();
  const titleId = useId();
  const approval = approvals?.find((a) => a.scope === "execute" && a.planRevision === revision);
  const replaced = revision < currentRevision;
  const H = level === 2 ? "h3" : "h4";
  return (
    <article aria-labelledby={titleId} className={`card discussion-revision${replaced ? "" : " discussion-revision-current"}`}>
      <H id={titleId}>{t("planDiscussion.revisionTitle", { n: revision })}</H>
      <p>{t("planDiscussion.revisionCreated", { n: revision })}</p>
      {replaced ? (
        <p className="muted">{t("planDiscussion.revisionReplaced", { n: currentRevision })}</p>
      ) : approval?.state === "pending" ? (
        <p>
          <strong>{t("planDiscussion.reapprove", { n: revision })}</strong>
        </p>
      ) : null}
      <p className="discussion-meta">
        {approval && revision > 1 ? (
          <Link to={`/approvals/${encodeURIComponent(approval.id)}`}>{t("planDiscussion.diffLink", { n: revision - 1 })}</Link>
        ) : (
          !replaced && <Link to={`/tasks/${encodeURIComponent(taskId)}#task-plan`}>{t("planDiscussion.planLink", { n: revision })}</Link>
        )}
        <span className="muted">{at}</span>
      </p>
    </article>
  );
}

function RetryButton({ taskId, id }: { taskId: string; id: string }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const retry = useRetryMessage(taskId);
  const [error, setError] = useState<LimitHit | string>();
  return (
    <div>
      <button type="button" className="button" disabled={retry.isPending}
        onClick={() => {
          setError(undefined);
          retry.mutate(id, {
            onSuccess: () => notify(t("planDiscussion.retried")),
            onError: (e) => setError(limitHit(e) ?? errorMessage(t, e)),
          });
        }}>
        {retry.isPending ? t("planDiscussion.retrying") : t("planDiscussion.retry")}
      </button>
      {typeof error === "object" ? <LimitAlert hit={error} /> : <FormAlert message={error} />}
    </div>
  );
}

function LimitAlert({ hit }: { hit: LimitHit }) {
  const { t, i18n } = useTranslation();
  const minutes = Math.max(1, Math.ceil(hit.retryAfterSec / 60));
  return (
    <p role="alert" className="form-alert">
      {t(`planDiscussion.limits.${hit.code}`, { limit: hit.limit, minutes })}
      {hit.at && <> {t("planDiscussion.retryAt", { at: formatDateTime(hit.at, i18n.language) })}</>}
    </p>
  );
}

const chars = (s: string) => [...s].length;

function Composer({ taskId, th }: { taskId: string; th: DiscussionThread }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const post = usePostMessage(taskId);
  const [text, setText] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const [error, setError] = useState<LimitHit | string>();
  const ref = useRef<HTMLTextAreaElement>(null);
  const max = th.limits.maxMessageChars;
  const count = chars(text);

  const fieldError = (v: string) => {
    const n = chars(v.trim());
    if (n === 0) return t("planDiscussion.required");
    if (n > max) return t("planDiscussion.limits.message_too_long", { limit: max });
    return undefined;
  };
  const invalid = submitted ? fieldError(text) : undefined;

  const submit = (e?: FormEvent) => {
    e?.preventDefault();
    if (post.isPending) return;
    setSubmitted(true);
    setError(undefined);
    if (fieldError(text)) {
      ref.current?.focus();
      return;
    }
    post.mutate(text.trim(), {
      onSuccess: () => {
        setText("");
        setSubmitted(false);
        notify(t("planDiscussion.sent"));
      },
      onError: (err) => setError(limitHit(err) ?? errorMessage(t, err)),
    });
  };
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) submit();
  };

  return (
    <form noValidate className="discussion-composer" onSubmit={submit}>
      <FormAlert message={invalid ? t("form.summary", { count: 1 }) : typeof error === "string" ? error : undefined} />
      {typeof error === "object" && <LimitAlert hit={error} />}
      <TextAreaField
        ref={ref}
        label={t("planDiscussion.composerLabel")}
        hint={t("planDiscussion.composerHint")}
        error={invalid}
        rows={4}
        value={text}
        onKeyDown={onKeyDown}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="discussion-actions">
        <span className={`discussion-counter${count > max ? " over" : ""}`}>
          {t("planDiscussion.counter", { n: count, max })}
        </span>
        <button type="submit" className="button primary" disabled={post.isPending}>
          {post.isPending ? t("planDiscussion.sending") : t("planDiscussion.send")}
        </button>
      </div>
    </form>
  );
}
