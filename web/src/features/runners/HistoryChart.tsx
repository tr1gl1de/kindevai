import { useState, type PointerEvent } from "react";
import { useTranslation } from "react-i18next";
import type { StorageSample } from "../../api/types";
import { formatBytes, formatDateTime } from "../../ui/format";

const HOUR = 3_600_000;
/** Levels of KB KIN-A-26, drawn as labelled reference lines (never colour alone). */
const LEVELS = [
  { percent: 75, key: "level_warning", cls: "warning" },
  { percent: 85, key: "level_high", cls: "high" },
  { percent: 95, key: "level_critical", cls: "critical" },
] as const;

const usedPercent = (s: StorageSample) => (s.totalBytes > 0 ? (100 * s.usedBytes) / s.totalBytes : 0);

/**
 * Fill history of one volume (KIN-55): used % over the window with the 75/85/95% levels. The plot stretches to any
 * width (320 px included) with non-scaling strokes; labels are HTML so they never shrink. Gaps over 2 h (the machine
 * was off) break the line. Pointer users get a crosshair with the nearest hour; everyone gets the summary as the
 * image label and the daily values as a table.
 */
export function HistoryChart({ samples, days, role }: { samples: StorageSample[]; days: number; role: string }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const [hover, setHover] = useState<number | null>(null);
  const pts = samples.map((s) => ({ s, t: Date.parse(s.at), p: usedPercent(s) }));
  const first = pts[0];
  const last = pts[pts.length - 1];
  if (!first || !last) return null;
  const end = last.t;
  const start = end - days * 24 * HOUR;
  const x = (ms: number) => (100 * (ms - start)) / (end - start || 1);
  const y = (p: number) => 100 - p;
  let d = "";
  let prev: number | null = null;
  for (const pt of pts) {
    const gap = prev === null || pt.t - prev > 2 * HOUR;
    d += `${gap ? "M" : "L"}${x(pt.t).toFixed(3)},${y(pt.p).toFixed(3)}`;
    prev = pt.t;
  }
  const label = t("storagePage.chartLabel", {
    role,
    count: days,
    from: Math.round(first.p),
    to: Math.round(last.p),
  });

  const onMove = (e: PointerEvent<HTMLDivElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    if (r.width <= 0) return;
    const at = start + ((e.clientX - r.left) / r.width) * (end - start);
    let best = 0;
    pts.forEach((pt, i) => {
      if (Math.abs(pt.t - at) < Math.abs((pts[best]?.t ?? 0) - at)) best = i;
    });
    setHover(best);
  };
  const h = hover === null ? null : (pts[hover] ?? null);

  return (
    <figure className="history-chart">
      <div className="history-plot" onPointerMove={onMove} onPointerLeave={() => setHover(null)}>
        <svg viewBox="0 0 100 100" preserveAspectRatio="none" role="img" aria-label={label} focusable="false">
          {[0, 25, 50, 100].map((p) => (
            <line key={p} className="grid" x1="0" x2="100" y1={y(p)} y2={y(p)} vectorEffect="non-scaling-stroke" />
          ))}
          {LEVELS.map((l) => (
            <line key={l.percent} className={`threshold ${l.cls}`} x1="0" x2="100" y1={y(l.percent)} y2={y(l.percent)}
              vectorEffect="non-scaling-stroke" />
          ))}
          <path className="series" d={d} vectorEffect="non-scaling-stroke" />
          {h && <line className="crosshair" x1={x(h.t)} x2={x(h.t)} y1="0" y2="100" vectorEffect="non-scaling-stroke" />}
        </svg>
        <span className="dot" style={{ left: `${x(last.t)}%`, top: `${y(last.p)}%` }} aria-hidden="true" />
        {LEVELS.map((l) => (
          <span key={l.percent} className="threshold-label" style={{ top: `${y(l.percent)}%` }} aria-hidden="true">
            {l.percent}%
          </span>
        ))}
        {h && (
          <span className="chart-tooltip" style={{ left: `${Math.min(Math.max(x(h.t), 20), 80)}%` }} aria-hidden="true">
            {t("storagePage.tooltip", {
              date: formatDateTime(h.s.at, lng),
              percent: Math.round(h.p),
              free: formatBytes(h.s.totalBytes - h.s.usedBytes, lng),
            })}
          </span>
        )}
      </div>
      <figcaption className="history-axis muted" aria-hidden="true">
        <span>{formatDateTime(new Date(start).toISOString(), lng)}</span>
        <span>{formatDateTime(last.s.at, lng)}</span>
      </figcaption>
      <p className="muted small">{t("storagePage.levelsLegend")}</p>
      <details>
        <summary>{t("storagePage.showData")}</summary>
        <DailyTable samples={samples} role={role} />
      </details>
    </figure>
  );
}

/** The last sample of each day, newest first. */
function DailyTable({ samples, role }: { samples: StorageSample[]; role: string }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const byDay = new Map<string, StorageSample>();
  for (const s of samples) byDay.set(s.at.slice(0, 10), s);
  const rows = [...byDay.entries()].reverse();
  const day = new Intl.DateTimeFormat(lng, { dateStyle: "medium", timeZone: "UTC" });
  return (
    <div className="table-wrap">
      <table>
        <caption className="visually-hidden">{t("storagePage.dataTable", { role })}</caption>
        <thead>
          <tr>
            <th scope="col">{t("storagePage.colDay")}</th>
            <th scope="col">{t("storagePage.colUsed")}</th>
            <th scope="col">{t("storagePage.colFree")}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(([k, s]) => (
            <tr key={k}>
              <th scope="row">{day.format(new Date(s.at))}</th>
              <td>{Math.round(usedPercent(s))}%</td>
              <td>{formatBytes(s.totalBytes - s.usedBytes, lng)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
