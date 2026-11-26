/** Date and time in the UI language, e.g. "24 Nov 2026, 15:00" / "24 нояб. 2026 г., 15:00". */
export function formatDateTime(iso: string, lng: string): string {
  return new Intl.DateTimeFormat(lng, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso));
}

/** Bytes in decimal units of the UI language, e.g. "4 GB" / "4 ГБ" (small sizes in kB or bytes, never "0 MB"). */
export function formatBytes(bytes: number, lng: string): string {
  const units = [
    ["terabyte", 1e12],
    ["gigabyte", 1e9],
    ["megabyte", 1e6],
    ["kilobyte", 1e3],
    ["byte", 1],
  ] as const;
  const [unit, size] = units.find(([, s]) => bytes >= s) ?? units[4];
  return new Intl.NumberFormat(lng, { style: "unit", unit, maximumFractionDigits: 1 }).format(bytes / size);
}

/** Duration in the UI language, e.g. "1 hr 5 min" / "1 ч 5 мин"; under a minute in seconds ("42 sec"). */
export function formatDuration(ms: number, lng: string): string {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const unit = (n: number, u: "hour" | "minute" | "second") =>
    new Intl.NumberFormat(lng, { style: "unit", unit: u, unitDisplay: "short" }).format(n);
  if (h > 0) return m > 0 ? `${unit(h, "hour")} ${unit(m, "minute")}` : unit(h, "hour");
  if (m > 0) return s > 0 ? `${unit(m, "minute")} ${unit(s, "second")}` : unit(m, "minute");
  return unit(s, "second");
}
