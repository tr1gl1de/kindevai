import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link, useSearchParams } from "react-router";
import type { Project, TaskListItem, TaskState } from "../../api/types";
import { SelectField, TextField } from "../../ui/Field";
import { EmptyState, ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage, validationMessage } from "../../ui/errors";
import { formatDateTime } from "../../ui/format";
import { useFieldErrors } from "../../ui/form";
import { useNotify } from "../../ui/notify";
import { isApiError } from "../../api/client";
import { useProjects } from "../projects/api";
import { useCreateTask, useTasks } from "./api";
import { AgentTimeValue, CalendarValue, TaskStateBadge } from "./badges";
import "./tasks.css";

export const taskStates: TaskState[] = ["CREATED", "ANALYZING", "READY", "AWAITING_APPROVAL", "RUNNING", "VERIFYING",
  "BLOCKED", "COMPLETED", "FAILED", "CANCELLED"];

/** Tasks & Executions: the visible projects' tasks, filtered by project and state (kept in the URL). */
export function TasksPage() {
  const { t } = useTranslation();
  const [params, setParams] = useSearchParams();
  const projectId = params.get("projectId") ?? "";
  const rawState = params.get("state") ?? "";
  const state = (taskStates as string[]).includes(rawState) ? (rawState as TaskState) : "";
  const projects = useProjects();
  const list = useTasks({ projectId, state });
  const names = new Map((projects.data ?? []).map((p) => [p.id, p.name]));
  const manageable = (projects.data ?? []).filter((p) => p.myRole === "owner" || p.myRole === "project_admin");

  const setFilter = (key: "projectId" | "state", value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };
  const filtered = projectId !== "" || state !== "";

  return (
    <>
      <PageHeading>{t("tasks.title")}</PageHeading>
      <p className="muted">{t("tasks.intro")}</p>
      <div className="task-filters">
        <SelectField label={t("tasks.filterProject")} value={projectId} onChange={(e) => setFilter("projectId", e.target.value)}>
          <option value="">{t("tasks.allProjects")}</option>
          {(projects.data ?? []).map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </SelectField>
        <SelectField label={t("tasks.filterState")} value={state} onChange={(e) => setFilter("state", e.target.value)}>
          <option value="">{t("tasks.allStates")}</option>
          {taskStates.map((s) => (
            <option key={s} value={s}>
              {t(`tasks.state_${s}`)}
            </option>
          ))}
        </SelectField>
      </div>
      {list.isPending ? (
        <Loading />
      ) : list.isError ? (
        <ErrorState message={errorMessage(t, list.error)} onRetry={() => void list.refetch()} />
      ) : (
        <>
          <section aria-labelledby="tasks-total" className="card task-total">
            <h2 id="tasks-total">{t("tasks.totalTitle")}</h2>
            <p className="task-total-value">
              <AgentTimeValue ms={list.data.total.ms} status={list.data.total.status} />
            </p>
            <p className="muted">{t("tasks.totalHint")}</p>
            {list.data.total.status === "incomplete" && <p>{t("tasks.incompleteHint")}</p>}
          </section>
          {list.data.tasks.length === 0 ? (
            <EmptyState title={t(filtered ? "tasks.emptyFiltered" : "tasks.emptyAll")}>
              <p>{t(filtered ? "tasks.emptyFilteredText" : "tasks.emptyAllText")}</p>
            </EmptyState>
          ) : (
            <TaskTable items={list.data.tasks} names={names} />
          )}
        </>
      )}
      {manageable.length > 0 && <CreateTask projects={manageable} preselect={projectId} />}
    </>
  );
}

function TaskTable({ items, names }: { items: TaskListItem[]; names: Map<string, string> }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="table-wrap section">
      <table>
        <thead>
          <tr>
            <th scope="col">{t("tasks.colTask")}</th>
            <th scope="col">{t("tasks.colProject")}</th>
            <th scope="col">{t("tasks.colState")}</th>
            <th scope="col">{t("tasks.colRevision")}</th>
            <th scope="col">{t("tasks.colAgentTime")}</th>
            <th scope="col">{t("tasks.colCalendar")}</th>
            <th scope="col">{t("tasks.colCreated")}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((task) => (
            <tr key={task.id}>
              <td>
                <Link to={`/tasks/${encodeURIComponent(task.id)}`}>{task.title}</Link>
                {task.youtrackIssue && <div className="muted">{task.youtrackIssue}</div>}
              </td>
              <td>{names.get(task.projectId) ?? task.projectId}</td>
              <td>
                <TaskStateBadge state={task.state} />
                {task.blockReason && <div className="muted">{t(`tasks.block_${task.blockReason}`)}</div>}
              </td>
              <td>{task.planRevision || "—"}</td>
              <td>
                <AgentTimeValue ms={task.time.agentMs} status={task.time.agentStatus} />
              </td>
              <td>
                <CalendarValue time={task.time} />
              </td>
              <td>{formatDateTime(task.createdAt, i18n.language)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const fields = ["title"] as const;

/** Manual task in a project the user manages (Owner or project admin). */
function CreateTask({ projects, preselect }: { projects: Project[]; preselect: string }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const create = useCreateTask();
  const [title, setTitle] = useState("");
  const [projectId, setProjectId] = useState(projects.some((p) => p.id === preselect) ? preselect : (projects[0]?.id ?? ""));
  const [formError, setFormError] = useState<string>();
  const { errors, show, clear, register } = useFieldErrors(fields);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setFormError(undefined);
    if (!title.trim()) {
      show({ title: t("form.required") });
      return;
    }
    create.mutate(
      { projectId, title: title.trim() },
      {
        onSuccess: (d) => {
          notify(t("tasks.created", { title: d.task.title }));
          setTitle("");
        },
        onError: (err) => {
          const v = isApiError(err) && err.field === "title" ? validationMessage(t, err.detail) : undefined;
          if (v) show({ title: v });
          else setFormError(errorMessage(t, err));
        },
      },
    );
  };

  return (
    <section aria-labelledby="task-create" className="card form-card section">
      <h2 id="task-create">{t("tasks.createTitle")}</h2>
      <form onSubmit={submit} noValidate>
        <FormAlert message={formError} />
        <TextField
          ref={register("title")}
          label={t("tasks.taskTitle")}
          hint={t("tasks.taskTitleHint")}
          error={errors.title}
          value={title}
          maxLength={200}
          autoComplete="off"
          onChange={(e) => {
            setTitle(e.target.value);
            clear("title");
          }}
        />
        {projects.length > 1 && (
          <SelectField label={t("tasks.project")} value={projectId} onChange={(e) => setProjectId(e.target.value)}>
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </SelectField>
        )}
        <div>
          <button type="submit" className="button primary" disabled={create.isPending}>
            {create.isPending ? t("tasks.creating") : t("tasks.create")}
          </button>
        </div>
      </form>
    </section>
  );
}
