import { useTranslation } from "react-i18next";
import type { RunnerOutputStorage, StorageMachine } from "../../api/types";
import { formatBytes, formatDateTime } from "../../ui/format";
import "./runners.css";

// Runner output stored in the Control Plane's database against its limit (KIN-52, KIN-59, KB KIN-A-18).

const badgeLevel = { ok: "ok", high: "high", full: "critical" } as const;

function Usage({ o }: { o: RunnerOutputStorage }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const pct = Math.min(100, Math.round(o.usedPercent));
  return (
    <>
      <meter min={0} max={100} low={75} high={90} optimum={0} value={pct} aria-label={t("storageOutput.meterLabel", { percent: pct })} />
      <p>
        <span className={`badge level-badge level-${badgeLevel[o.state]}`}>{t(`storageOutput.state_${o.state}`)}</span>{" "}
        {t("storageOutput.used", { used: formatBytes(o.usedBytes, lng), limit: formatBytes(o.limitBytes, lng), percent: pct })}
      </p>
    </>
  );
}

/** The warning while output is being dropped, otherwise a note about drops in the window (if any). */
function Drops({ o }: { o: RunnerOutputStorage }) {
  const { t, i18n } = useTranslation();
  const date = o.lastDroppedAt ? formatDateTime(o.lastDroppedAt, i18n.language) : "";
  if (o.state === "full") {
    return <p className="warning">{t("storageOutput.dropping", { date })}</p>;
  }
  if (o.droppedEvents > 0) {
    return <p className="muted">{t("storageOutput.droppedRecently", { count: o.droppedEvents, hours: o.dropsWindowHours, date })}</p>;
  }
  return null;
}

/** Compact block of the dashboard's Storage card. */
export function OutputStorageSummary({ o }: { o: RunnerOutputStorage }) {
  const { t } = useTranslation();
  return (
    <div className={`volume output-storage state-${o.state}`}>
      <p className="volume-head">
        <span className="volume-role">{t("storageOutput.title")}</span>
      </p>
      <Usage o={o} />
      <Drops o={o} />
    </div>
  );
}

/** Infrastructure → Storage: usage, retention settings and the attempts whose output was dropped recently. */
export function OutputStorageSection({ o, machines }: { o: RunnerOutputStorage; machines: StorageMachine[] }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const names = new Map(machines.map((m) => [m.id, m.name]));
  return (
    <section className={`card volume output-storage state-${o.state}`} aria-labelledby="storage-output">
      <h2 id="storage-output">{t("storageOutput.title")}</h2>
      <p className="muted">{t("storageOutput.intro")}</p>
      <Usage o={o} />
      <Drops o={o} />
      <dl className="details">
        <div className="details-row">
          <dt>{t("storageOutput.limit")}</dt>
          <dd>
            {t("storageOutput.limitValue", { size: formatBytes(o.limitBytes, lng), mib: Math.round(o.limitBytes / 2 ** 20) })}{" "}
            <code className="muted small">--runner-output-limit-mb</code>
          </dd>
        </div>
        <div className="details-row">
          <dt>{t("storageOutput.outputRetention")}</dt>
          <dd>
            {t("storageOutput.daysAfterFinish", { count: o.outputRetentionDays })}{" "}
            <code className="muted small">--runner-output-retention-days</code>
          </dd>
        </div>
        <div className="details-row">
          <dt>{t("storageOutput.eventRetention")}</dt>
          <dd>
            {t("storageOutput.daysAfterFinish", { count: o.eventRetentionDays })}{" "}
            <code className="muted small">--runner-event-retention-days</code>
          </dd>
        </div>
      </dl>
      <p className="muted small">{t("storageOutput.policy")}</p>
      <h3 id="storage-output-drops">{t("storageOutput.dropsTitle", { hours: o.dropsWindowHours })}</h3>
      {o.drops.length === 0 ? (
        <p className="muted">{t("storageOutput.noDrops", { hours: o.dropsWindowHours })}</p>
      ) : (
        <div className="table-wrap">
          <table className="compact" aria-labelledby="storage-output-drops">
            <thead>
              <tr>
                <th scope="col">{t("storageOutput.colRunner")}</th>
                <th scope="col">{t("storageOutput.colStep")}</th>
                <th scope="col">{t("storageOutput.colEvents")}</th>
                <th scope="col">{t("storageOutput.colLast")}</th>
              </tr>
            </thead>
            <tbody>
              {o.drops.map((d) => (
                <tr key={`${d.stepId}/${d.fence}/${d.runnerId}`}>
                  <th scope="row">{names.get(d.runnerId) || d.runnerId}</th>
                  <td>
                    <code>{d.stepId}</code> <span className="muted small">{t("storageOutput.attempt", { fence: d.fence })}</span>
                  </td>
                  <td>{new Intl.NumberFormat(lng).format(d.events)}</td>
                  <td>{formatDateTime(d.lastAt, lng)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {o.droppedEvents > 0 && (
        <p className="muted small">
          {t("storageOutput.droppedTotal", { count: o.droppedEvents })}
        </p>
      )}
    </section>
  );
}
