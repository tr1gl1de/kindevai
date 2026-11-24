// Telegram Mini App launch data, read without Telegram's SDK script (the CSP allows only same-origin scripts).
// Telegram opens the app with `#tgWebAppData=<initData>&tgWebAppThemeParams=<json>&…` in the URL fragment.
// initData is passed to the server verbatim; only the server's signature check makes it trustworthy. Values read
// here (user name, language, start parameter) are used for display and routing only.

export interface TelegramLaunch {
  /** Raw signed initData, sent to the server as is. */
  initData: string;
  /** startapp parameter (signed as part of initData), e.g. "link_…" or "invite_…". */
  startParam: string;
  /** Telegram client language, for the first-run UI language only. */
  languageCode: string;
}

const STORAGE_KEY = "kindevai.tg-launch";

/** Parses launch data from a URL fragment; null outside Telegram. */
export function parseLaunch(hash: string): TelegramLaunch | null {
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  const initData = params.get("tgWebAppData");
  if (!initData) return null;
  const data = new URLSearchParams(initData);
  let languageCode = "";
  try {
    const user = JSON.parse(data.get("user") ?? "{}") as { language_code?: unknown };
    if (typeof user.language_code === "string") languageCode = user.language_code;
  } catch {
    // unsigned display data only; ignore malformed input
  }
  return { initData, startParam: data.get("start_param") ?? "", languageCode };
}

function session(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

/**
 * Launch data of this tab: from the fragment on first load (then removed from the address bar), from session
 * storage after a reload. Null in a normal browser tab.
 */
export function readLaunch(loc: Location = window.location): TelegramLaunch | null {
  const fresh = parseLaunch(loc.hash);
  const store = session();
  if (fresh) {
    try {
      store?.setItem(STORAGE_KEY, JSON.stringify(fresh));
    } catch {
      // storage full or blocked: the launch still works for this page view
    }
    applyThemeParams(new URLSearchParams(loc.hash.slice(1)).get("tgWebAppThemeParams"));
    window.history.replaceState(window.history.state, "", loc.pathname + loc.search);
    return fresh;
  }
  try {
    const saved = store?.getItem(STORAGE_KEY);
    return saved ? (JSON.parse(saved) as TelegramLaunch) : null;
  } catch {
    return null;
  }
}

const colorRe = /^#[0-9a-fA-F]{3,8}$/;

/** Sets --tg-theme-* variables (which the design tokens prefer) from Telegram's theme params JSON. */
export function applyThemeParams(json: string | null, root: HTMLElement = document.documentElement) {
  if (!json) return;
  let params: Record<string, unknown>;
  try {
    params = JSON.parse(json) as Record<string, unknown>;
  } catch {
    return;
  }
  for (const [key, value] of Object.entries(params)) {
    if (/^[a-z_]+$/.test(key) && typeof value === "string" && colorRe.test(value)) {
      root.style.setProperty(`--tg-theme-${key.replaceAll("_", "-")}`, value);
    }
  }
}

interface TelegramHost {
  TelegramWebviewProxy?: { postEvent(type: string, data: string): void };
}

/** Tells the Telegram client the app is ready and may take the full height (no-op outside Telegram). */
export function signalReady() {
  const events: [string, object][] = [
    ["web_app_ready", {}],
    ["web_app_expand", {}],
  ];
  const host = window as unknown as TelegramHost;
  for (const [type, data] of events) {
    try {
      if (host.TelegramWebviewProxy) host.TelegramWebviewProxy.postEvent(type, JSON.stringify(data));
      else if (window.parent !== window)
        window.parent.postMessage(JSON.stringify({ eventType: type, eventData: data }), "https://web.telegram.org");
    } catch {
      // the client ignores unknown hosts; nothing to do
    }
  }
}

/** First Mini App start: use the Telegram language unless the user already picked one. */
export function seedLocale(launch: TelegramLaunch | null, storageKey: string) {
  if (!launch?.languageCode) return;
  try {
    if (window.localStorage.getItem(storageKey)) return;
    window.localStorage.setItem(storageKey, launch.languageCode.toLowerCase().startsWith("ru") ? "ru" : "en");
  } catch {
    // storage blocked: the browser language is used instead
  }
}

/** Splits a start parameter into its kind and token. */
export function startTarget(startParam: string): { kind: "link" | "invite"; token: string } | null {
  const m = /^(link|invite)_([A-Za-z0-9_-]{1,57})$/.exec(startParam);
  return m ? { kind: m[1] as "link" | "invite", token: m[2]! } : null;
}
