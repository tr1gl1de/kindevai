import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { isApiError } from "../../api/client";
import type { DnsPolicy, Enforcement, NetworkAssignment, NetworkLevel, NetworkProfile, NetworkRule, NetworkTest } from "../../api/types";
import { ConfirmDialog } from "../../ui/ConfirmDialog";
import { SelectField, TextAreaField, TextField } from "../../ui/Field";
import { EmptyState, ErrorState, FormAlert, Loading, PageHeading } from "../../ui/feedback";
import { errorMessage } from "../../ui/errors";
import { useFieldErrors } from "../../ui/form";
import { formatDateTime } from "../../ui/format";
import { useNotify } from "../../ui/notify";
import { useProjects } from "../projects/api";
import { useRunners } from "../runners/api";
import {
  useAssignNetworkProfile,
  useDeleteNetworkProfile,
  useNetworkAssignments,
  useNetworkProfiles,
  useNetworkTests,
  useRequestNetworkTest,
  useSaveNetworkProfile,
} from "./api";

/** CLI roles a profile can be assigned to (the most specific level). */
export const cliRoles = ["claude", "codex", "zcode"] as const;

const formFields = ["name", "proxy", "password", "expectedIpv4", "expectedIpv6", "diagnosticUrl", "allow"] as const;
type FormField = (typeof formFields)[number];

const knownCodes = new Set([
  "required",
  "name_format",
  "proxy_scheme",
  "proxy_format",
  "proxy_host",
  "proxy_port",
  "proxy_username",
  "password_format",
  "strict_requires_proxy",
  "ip_format",
  "ip_family",
  "ip_not_public",
  "diagnostic_url",
  "allow_rule",
  "allow_host",
  "allow_cidr",
  "allow_cidr_too_wide",
  "allow_cidr_forbidden",
  "allow_ports",
  "allow_too_many",
  "name_taken",
  "runner_inactive",
  "test_pending",
]);

function codeMessage(t: TFunction, code: string | undefined): string {
  return code && knownCodes.has(code) ? t(`network.v_${code}` as "network.v_required") : t("errors.unknown");
}

/** The server's field names mapped onto the form's. */
function formField(field: string | undefined): FormField | undefined {
  if (field === "password" || field === "proxy") return field;
  if (field === "expectedIpv4" || field === "expectedIpv6" || field === "diagnosticUrl" || field === "allow" || field === "name")
    return field;
  return undefined;
}

/** "host-or-cidr ports" per line, e.g. "*.github.com 443,22". */
export function parseAllow(text: string): NetworkRule[] | undefined {
  const rules: NetworkRule[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const [dest, ports, extra] = line.split(/\s+/);
    if (!dest || !ports || extra) return undefined;
    const nums = ports.split(",").map((p) => Number(p));
    if (nums.some((n) => !Number.isInteger(n) || n < 1 || n > 65535)) return undefined;
    const isCidr = dest.includes("/") || /^[\d.]+$/.test(dest) || dest.includes(":");
    rules.push(isCidr ? { cidr: dest, ports: nums } : { host: dest, ports: nums });
  }
  return rules;
}

function formatAllow(rules: NetworkRule[]): string {
  return rules.map((r) => `${r.host ?? r.cidr ?? ""} ${r.ports.join(",")}`).join("\n");
}

/** Owner-only: network profiles, where they apply, and Test Network. */
export function NetworkPage() {
  const { t } = useTranslation();
  const profiles = useNetworkProfiles();
  const [editing, setEditing] = useState<NetworkProfile | "new" | null>(null);
  return (
    <>
      <PageHeading>{t("network.title")}</PageHeading>
      <p className="muted">{t("network.intro")}</p>
      <section aria-labelledby="profiles-title" className="section">
        <h2 id="profiles-title">{t("network.profiles")}</h2>
        {profiles.isPending && <Loading />}
        {profiles.isError && <ErrorState message={errorMessage(t, profiles.error)} onRetry={() => void profiles.refetch()} />}
        {profiles.isSuccess && profiles.data.length === 0 && editing === null && (
          <EmptyState title={t("network.noProfiles")}>
            <p>{t("network.noProfilesHint")}</p>
          </EmptyState>
        )}
        {profiles.isSuccess && profiles.data.length > 0 && <ProfileList profiles={profiles.data} onEdit={setEditing} />}
        {editing === null ? (
          <div className="form-actions">
            <button type="button" className="button primary" onClick={() => setEditing("new")}>
              {t("network.newProfile")}
            </button>
          </div>
        ) : (
          <ProfileForm
            key={editing === "new" ? "new" : editing.id}
            profile={editing === "new" ? null : editing}
            onDone={() => setEditing(null)}
          />
        )}
      </section>
      {profiles.isSuccess && <Assignments profiles={profiles.data} />}
      {profiles.isSuccess && <TestNetwork profiles={profiles.data} />}
    </>
  );
}

function ProfileList({ profiles, onEdit }: { profiles: NetworkProfile[]; onEdit: (p: NetworkProfile) => void }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const del = useDeleteNetworkProfile();
  const [confirm, setConfirm] = useState<NetworkProfile | null>(null);
  const [error, setError] = useState<string>();

  async function doDelete(p: NetworkProfile) {
    setError(undefined);
    try {
      await del.mutateAsync(p.id);
      notify(t("network.deleted", { name: p.name }));
    } catch (err) {
      setError(isApiError(err, "in_use") ? t("network.inUse") : errorMessage(t, err));
    } finally {
      setConfirm(null);
    }
  }

  return (
    <>
      <FormAlert message={error} />
      <ul className="card-list">
        {profiles.map((p) => (
          <li key={p.id} className="card">
            <h3>{p.name}</h3>
            <dl className="details">
              <dt>{t("network.proxy")}</dt>
              <dd>{p.proxy?.url ?? t("network.direct")}</dd>
              <dt>{t("network.password")}</dt>
              <dd>{p.passwordSet ? t("network.passwordSet") : t("network.passwordNone")}</dd>
              <dt>{t("network.enforcement")}</dt>
              <dd>
                {t(`network.enforcement_${p.enforcement}`)}
                {p.failClosed ? `, ${t("network.failClosedShort")}` : ""}
              </dd>
              <dt>{t("network.expectedIp")}</dt>
              <dd>{[p.expectedIpv4, p.expectedIpv6].filter(Boolean).join(", ") || "—"}</dd>
              <dt>{t("network.assignedTo")}</dt>
              <dd>
                {p.assigned.length
                  ? p.assigned
                      .map((a) => (a.level === "system" ? t("network.level_system") : `${t(`network.level_${a.level}`)}: ${a.target}`))
                      .join("; ")
                  : t("network.unassigned")}
              </dd>
            </dl>
            <div className="form-actions">
              <button type="button" className="button" onClick={() => onEdit(p)} aria-label={t("network.editNamed", { name: p.name })}>
                {t("network.edit")}
              </button>
              <button
                type="button"
                className="button danger"
                onClick={() => setConfirm(p)}
                aria-label={t("network.deleteNamed", { name: p.name })}
              >
                {t("network.delete")}
              </button>
            </div>
          </li>
        ))}
      </ul>
      {confirm && (
        <ConfirmDialog
          title={t("network.deleteTitle", { name: confirm.name })}
          text={t("network.deleteText")}
          confirmLabel={t("network.delete")}
          cancelLabel={t("users.cancel")}
          busy={del.isPending}
          onConfirm={() => void doDelete(confirm)}
          onCancel={() => setConfirm(null)}
        />
      )}
    </>
  );
}

function ProfileForm({ profile, onDone }: { profile: NetworkProfile | null; onDone: () => void }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const save = useSaveNetworkProfile();
  const { errors, show, clear, register, count } = useFieldErrors(formFields);
  const [name, setName] = useState(profile?.name ?? "");
  const [proxyUrl, setProxyUrl] = useState(profile?.proxy?.url ?? "");
  const [password, setPassword] = useState("");
  const [clearPassword, setClearPassword] = useState(false);
  const [enforcement, setEnforcement] = useState<Enforcement>(profile?.enforcement ?? "off");
  const [dns, setDns] = useState<DnsPolicy>(profile?.dns ?? "system");
  const [failClosed, setFailClosed] = useState(profile?.failClosed ?? false);
  const [ipv4, setIpv4] = useState(profile?.expectedIpv4 ?? "");
  const [ipv6, setIpv6] = useState(profile?.expectedIpv6 ?? "");
  const [diag, setDiag] = useState(profile?.diagnosticUrl ?? "");
  const [allow, setAllow] = useState(formatAllow(profile?.allow ?? []));
  const [formError, setFormError] = useState<string>();
  const strict = enforcement === "strict";

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setFormError(undefined);
    const missing: Partial<Record<FormField, string>> = {};
    if (!name.trim()) missing.name = t("form.required");
    if (strict && !proxyUrl.trim()) missing.proxy = codeMessage(t, "strict_requires_proxy");
    const rules = parseAllow(allow);
    if (!rules) missing.allow = t("network.allowSyntax");
    if (Object.keys(missing).length || !rules) {
      show(missing);
      return;
    }
    try {
      await save.mutateAsync({
        id: profile?.id,
        body: {
          name: name.trim(),
          proxyUrl: proxyUrl.trim(),
          ...(password ? { password } : {}),
          clearPassword,
          enforcement,
          dns,
          failClosed,
          expectedIpv4: ipv4.trim(),
          expectedIpv6: ipv6.trim(),
          allow: rules,
          diagnosticUrl: diag.trim(),
        },
      });
      setPassword("");
      save.reset(); // drop the request (with the password) from the mutation cache
      notify(t("network.saved", { name: name.trim() }));
      onDone();
    } catch (err) {
      save.reset();
      const f = isApiError(err, "validation") ? formField(err.field) : undefined;
      if (f && isApiError(err)) show({ [f]: codeMessage(t, err.detail) });
      else if (isApiError(err, "name_taken")) show({ name: codeMessage(t, "name_taken") });
      else setFormError(errorMessage(t, err));
    }
  }

  return (
    <form
      noValidate
      onSubmit={onSubmit}
      aria-label={profile ? t("network.editTitle", { name: profile.name }) : t("network.newProfile")}
      className="card form-card"
    >
      <h3>{profile ? t("network.editTitle", { name: profile.name }) : t("network.newProfile")}</h3>
      <FormAlert message={count ? t("form.summary", { count }) : formError} />
      <TextField
        ref={register("name")}
        label={t("network.name")}
        error={errors.name}
        value={name}
        autoComplete="off"
        onChange={(e) => {
          setName(e.target.value);
          clear("name");
        }}
      />
      <TextField
        ref={register("proxy")}
        label={t("network.proxyUrl")}
        hint={t("network.proxyUrlHint")}
        error={errors.proxy}
        type="url"
        inputMode="url"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        value={proxyUrl}
        onChange={(e) => {
          setProxyUrl(e.target.value);
          clear("proxy");
        }}
      />
      <TextField
        ref={register("password")}
        label={t("network.password")}
        hint={profile?.passwordSet ? t("network.passwordKeepHint") : t("network.passwordHint")}
        keepHint
        error={errors.password}
        type="password"
        autoComplete="new-password"
        autoCapitalize="none"
        spellCheck={false}
        value={password}
        onChange={(e) => {
          setPassword(e.target.value);
          clear("password");
        }}
      />
      {profile?.passwordSet && (
        <label className="check">
          <input type="checkbox" checked={clearPassword} onChange={(e) => setClearPassword(e.target.checked)} />
          <span>{t("network.clearPassword")}</span>
        </label>
      )}
      <SelectField
        label={t("network.enforcement")}
        hint={t(`network.enforcementHint_${enforcement}`)}
        value={enforcement}
        onChange={(e) => setEnforcement(e.target.value as Enforcement)}
      >
        {(["off", "monitor", "strict"] as const).map((v) => (
          <option key={v} value={v}>
            {t(`network.enforcement_${v}`)}
          </option>
        ))}
      </SelectField>
      <SelectField
        label={t("network.dns")}
        hint={strict ? t("network.strictForces") : undefined}
        value={strict ? "proxy" : dns}
        disabled={strict}
        onChange={(e) => setDns(e.target.value as DnsPolicy)}
      >
        <option value="system">{t("network.dns_system")}</option>
        <option value="proxy">{t("network.dns_proxy")}</option>
      </SelectField>
      <label className="check">
        <input type="checkbox" checked={strict || failClosed} disabled={strict} onChange={(e) => setFailClosed(e.target.checked)} />
        <span>{t("network.failClosed")}</span>
      </label>
      <TextField
        ref={register("expectedIpv4")}
        label={t("network.expectedIpv4")}
        hint={t("network.expectedIpHint")}
        error={errors.expectedIpv4}
        inputMode="decimal"
        autoComplete="off"
        spellCheck={false}
        value={ipv4}
        onChange={(e) => {
          setIpv4(e.target.value);
          clear("expectedIpv4");
        }}
      />
      <TextField
        ref={register("expectedIpv6")}
        label={t("network.expectedIpv6")}
        error={errors.expectedIpv6}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        value={ipv6}
        onChange={(e) => {
          setIpv6(e.target.value);
          clear("expectedIpv6");
        }}
      />
      <TextField
        ref={register("diagnosticUrl")}
        label={t("network.diagnosticUrl")}
        hint={t("network.diagnosticUrlHint")}
        error={errors.diagnosticUrl}
        type="url"
        inputMode="url"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        value={diag}
        onChange={(e) => {
          setDiag(e.target.value);
          clear("diagnosticUrl");
        }}
      />
      <TextAreaField
        ref={register("allow")}
        label={t("network.allow")}
        hint={t("network.allowHint")}
        keepHint
        error={errors.allow}
        rows={4}
        autoCapitalize="none"
        spellCheck={false}
        value={allow}
        onChange={(e) => {
          setAllow(e.target.value);
          clear("allow");
        }}
      />
      <div className="form-actions">
        <button type="submit" className="button primary" disabled={save.isPending}>
          {save.isPending ? t("form.submitting") : t("network.save")}
        </button>
        <button type="button" className="button" onClick={onDone}>
          {t("users.cancel")}
        </button>
      </div>
    </form>
  );
}

function Assignments({ profiles }: { profiles: NetworkProfile[] }) {
  const { t } = useTranslation();
  const notify = useNotify();
  const assignments = useNetworkAssignments();
  const runners = useRunners();
  const projects = useProjects();
  const assign = useAssignNetworkProfile();
  const [error, setError] = useState<string>();

  const current = (level: NetworkLevel, target: string) =>
    assignments.data?.find((a) => a.level === level && a.target === target)?.profileId ?? "";

  async function change(a: NetworkAssignment) {
    setError(undefined);
    try {
      await assign.mutateAsync(a);
      notify(t("network.assignmentSaved"));
    } catch (err) {
      setError(errorMessage(t, err));
    }
  }

  const systemProfile = profiles.find((p) => p.id === current("system", ""));
  const rows: { level: NetworkLevel; target: string; label: string }[] = [
    ...(runners.data ?? [])
      .filter((r) => r.status === "approved" || r.status === "disabled")
      .map((r) => ({
        level: "runner" as const,
        target: r.id,
        label: t("network.runnerNamed", { name: r.hostname || r.id }),
      })),
    ...(projects.data ?? []).map((p) => ({
      level: "project" as const,
      target: p.id,
      label: t("network.projectNamed", { name: p.name }),
    })),
    ...cliRoles.map((r) => ({
      level: "role" as const,
      target: r,
      label: t("network.roleNamed", { name: r }),
    })),
  ];

  const select = (level: NetworkLevel, target: string, label: string) => (
    <SelectField
      key={`${level}:${target}`}
      label={label}
      value={current(level, target)}
      disabled={assign.isPending}
      onChange={(e) => void change({ level, target, profileId: e.target.value })}
    >
      <option value="">{level === "system" ? t("network.noneDirect") : t("network.inherit")}</option>
      {profiles.map((p) => (
        <option key={p.id} value={p.id}>
          {p.name}
        </option>
      ))}
    </SelectField>
  );

  return (
    <section aria-labelledby="assign-title" className="section">
      <h2 id="assign-title">{t("network.assignments")}</h2>
      <p className="muted">{t("network.assignmentsIntro")}</p>
      {assignments.isPending && <Loading />}
      {assignments.isError && <ErrorState message={errorMessage(t, assignments.error)} onRetry={() => void assignments.refetch()} />}
      {assignments.isSuccess && (
        <div className="card form-card">
          <FormAlert message={error} />
          {select("system", "", t("network.level_system"))}
          {systemProfile?.enforcement === "strict" && <p className="field-hint">{t("network.strictLocked")}</p>}
          {rows.map((r) => select(r.level, r.target, r.label))}
        </div>
      )}
    </section>
  );
}

function TestStatus({ test }: { test: NetworkTest }) {
  const { t } = useTranslation();
  return <strong>{t(`network.status_${test.status}`)}</strong>;
}

function TestNetwork({ profiles }: { profiles: NetworkProfile[] }) {
  const { t, i18n } = useTranslation();
  const runners = useRunners();
  const tests = useNetworkTests();
  const request = useRequestNetworkTest();
  const active = (runners.data ?? []).filter((r) => r.status === "approved");
  const [runnerId, setRunnerId] = useState("");
  const [profileId, setProfileId] = useState("");
  const [error, setError] = useState<string>();
  const runnerName = (id: string) => runners.data?.find((r) => r.id === id)?.hostname || id;
  const profileName = (id: string) => (id ? (profiles.find((p) => p.id === id)?.name ?? id) : t("network.effective"));

  async function onSubmit(ev: FormEvent) {
    ev.preventDefault();
    setError(undefined);
    const rid = runnerId || active[0]?.id;
    if (!rid) return;
    try {
      await request.mutateAsync({ runnerId: rid, profileId });
    } catch (err) {
      setError(isApiError(err, "validation") ? codeMessage(t, err.detail) : errorMessage(t, err));
    }
  }

  return (
    <section aria-labelledby="test-title" className="section">
      <h2 id="test-title">{t("network.test")}</h2>
      <p className="muted">{t("network.testIntro")}</p>
      {active.length === 0 ? (
        <EmptyState title={t("network.noRunners")} />
      ) : (
        <form noValidate onSubmit={(e) => void onSubmit(e)} aria-label={t("network.test")} className="card form-card">
          <FormAlert message={error} />
          <SelectField label={t("network.testRunner")} value={runnerId || active[0]!.id} onChange={(e) => setRunnerId(e.target.value)}>
            {active.map((r) => (
              <option key={r.id} value={r.id}>
                {r.hostname || r.id}
              </option>
            ))}
          </SelectField>
          <SelectField label={t("network.testProfile")} value={profileId} onChange={(e) => setProfileId(e.target.value)}>
            <option value="">{t("network.effective")}</option>
            {profiles.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </SelectField>
          <div className="form-actions">
            <button type="submit" className="button primary" disabled={request.isPending}>
              {request.isPending ? t("form.submitting") : t("network.runTest")}
            </button>
          </div>
        </form>
      )}
      <h3>{t("network.recentTests")}</h3>
      {tests.isPending && <Loading />}
      {tests.isError && <ErrorState message={errorMessage(t, tests.error)} onRetry={() => void tests.refetch()} />}
      {tests.isSuccess && tests.data.length === 0 && <p className="muted">{t("network.noTests")}</p>}
      {tests.isSuccess && tests.data.length > 0 && (
        <ul className="card-list" aria-live="polite">
          {tests.data.map((x) => (
            <li key={x.id} className="card" aria-busy={x.status === "pending"}>
              <p>
                <TestStatus test={x} />{" "}
                {t("network.testOn", {
                  runner: runnerName(x.runnerId),
                  profile: profileName(x.profileId),
                })}
              </p>
              <dl className="details">
                <dt>{t("network.observedIp")}</dt>
                <dd>{[x.observedIpv4, x.observedIpv6].filter(Boolean).join(", ") || "—"}</dd>
                <dt>{t("network.expectedIp")}</dt>
                <dd>{[x.expectedIpv4, x.expectedIpv6].filter(Boolean).join(", ") || "—"}</dd>
                {x.code && (
                  <>
                    <dt>{t("network.reason")}</dt>
                    <dd>
                      {reason(t, x.code)}
                      {x.detail ? ` (${x.detail})` : ""}
                    </dd>
                  </>
                )}
                <dt>{t("network.checkedAt")}</dt>
                <dd>{formatDateTime(x.completedAt ?? x.requestedAt, i18n.language)}</dd>
              </dl>
              {x.checks.length > 0 && (
                <ul className="checks">
                  {x.checks.map((c) => (
                    <li key={c.name}>
                      {c.ok ? t("network.checkOk") : t("network.checkFailed")} — {c.name} ({c.host})
                      {c.ok ? `, ${c.latencyMs} ms` : `: ${reason(t, c.code)}`}
                    </li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

const reasonCodes = new Set([
  "proxy_unreachable",
  "proxy_auth_failed",
  "strict_requires_proxy",
  "diagnostic_failed",
  "diagnostic_invalid_response",
  "egress_ip_mismatch",
  "egress_ip_unverified",
  "endpoint_unreachable",
  "timeout",
  "dns_failed",
  "tls_failed",
  "unreachable",
  "bad_url",
  "busy",
  "failed",
]);

function reason(t: TFunction, code: string): string {
  return reasonCodes.has(code) ? t(`network.r_${code}` as "network.r_failed") : code;
}
