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
/** Telegram's last theme params of this tab, so a reload (which loses the URL fragment) keeps its theme (KIN-114). */
const THEME_KEY = "kindevai.tg-theme";

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
    try {
      store?.removeItem(THEME_KEY); // a theme from an earlier launch in this tab no longer applies
    } catch {
      // storage blocked: nothing was saved either
    }
    applyThemeParams(new URLSearchParams(loc.hash.slice(1)).get("tgWebAppThemeParams"));
    window.history.replaceState(window.history.state, "", loc.pathname + loc.search);
    return fresh;
  }
  let saved: TelegramLaunch | null;
  try {
    const raw = store?.getItem(STORAGE_KEY);
    saved = raw ? (JSON.parse(raw) as TelegramLaunch) : null;
  } catch {
    return null;
  }
  if (saved) {
    try {
      applyThemeParams(store?.getItem(THEME_KEY) ?? null);
    } catch {
      // storage blocked: the theme follows on the client's next theme_changed
    }
  }
  return saved;
}

const colorRe = /^#[0-9a-fA-F]{3,8}$/;

/** Relative luminance (WCAG) of a #rgb, #rgba, #rrggbb or #rrggbbaa colour (alpha ignored). */
function luminance(hex: string): number {
  const h = hex.length <= 5 ? [...hex.slice(1, 4)].map((c) => c + c).join("") : hex.slice(1, 7);
  const [r, g, b] = [0, 2, 4].map((i) => {
    const c = parseInt(h.slice(i, i + 2), 16) / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
}

/**
 * Sets --tg-theme-* variables (which the design tokens prefer) from Telegram's theme params JSON, and data-theme
 * from its background, so the tokens Telegram does not provide follow its light or dark theme, not the OS scheme.
 * Telegram always sends a whole theme: variables an earlier theme set and this one does not are removed. Only colour
 * values are taken; the params are remembered for a reload of this tab.
 */
export function applyThemeParams(json: string | null, root: HTMLElement = document.documentElement) {
  if (!json) return;
  let params: unknown;
  try {
    params = JSON.parse(json);
  } catch {
    return;
  }
  if (!applyTheme(params, root)) return;
  try {
    window.sessionStorage.setItem(THEME_KEY, json);
  } catch {
    // storage full or blocked: the theme still applies to this page view
  }
}

/** Applies a theme params object; false (nothing changed) when it is not one. */
function applyTheme(params: unknown, root: HTMLElement): boolean {
  if (typeof params !== "object" || params === null || Array.isArray(params)) return false;
  const colours = new Map<string, string>();
  for (const [key, value] of Object.entries(params)) {
    if (/^[a-z_]+$/.test(key) && typeof value === "string" && colorRe.test(value)) {
      colours.set(`--tg-theme-${key.replaceAll("_", "-")}`, value);
    }
  }
  if (colours.size === 0) return false;
  for (const name of [...root.style].filter((p) => p.startsWith("--tg-theme-") && !colours.has(p))) {
    root.style.removeProperty(name);
  }
  for (const [name, value] of colours) root.style.setProperty(name, value);
  const bg = colours.get("--tg-theme-bg-color");
  if (bg && /^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/.test(bg)) {
    // Below 0.179 black text would contrast less than white text: a dark background.
    root.dataset.theme = luminance(bg) < 0.179 ? "dark" : "light";
  } else {
    // Without Telegram's background a light/dark choice from an earlier theme would be stale: follow the OS.
    delete root.dataset.theme;
  }
  return true;
}

type Receive = (eventType: string, eventData: unknown) => void;

interface TelegramHost {
  TelegramWebviewProxy?: { postEvent(type: string, data: string): void };
  Telegram?: { WebView?: { receiveEvent?: Receive } };
  TelegramGameProxy?: { receiveEvent?: Receive };
  TelegramGameProxy_receiveEvent?: Receive;
}

/** The web client (web.telegram.org) hosts the app in an iframe; signalReady and followTheme talk only to it. */
const WEB_CLIENT_ORIGIN = "https://web.telegram.org";

function postEvent(type: string, data: object) {
  const host = window as unknown as TelegramHost;
  try {
    if (host.TelegramWebviewProxy) host.TelegramWebviewProxy.postEvent(type, JSON.stringify(data));
    else if (window.parent !== window)
      window.parent.postMessage(JSON.stringify({ eventType: type, eventData: data }), WEB_CLIENT_ORIGIN);
  } catch {
    // the client ignores unknown hosts; nothing to do
  }
}

/** Tells the Telegram client the app is ready and may take the full height (no-op outside Telegram). */
export function signalReady() {
  postEvent("web_app_ready", {});
  postEvent("web_app_expand", {});
}

/**
 * Follows the theme the user picks in Telegram (KIN-114): applies the theme_changed event, which mobile and desktop
 * clients deliver by calling window.Telegram.WebView.receiveEvent (older ones TelegramGameProxy) and the web client
 * by postMessage from the parent frame, and asks the client for its current theme. Receivers another script set
 * keep getting every event. Returns a function that removes the listeners.
 */
export function followTheme(root: HTMLElement = document.documentElement): () => void {
  const host = window as unknown as TelegramHost;
  const onEvent = (eventType: string, eventData: unknown) => {
    if (eventType !== "theme_changed" || typeof eventData !== "object" || eventData === null) return;
    const params = (eventData as { theme_params?: unknown }).theme_params;
    if (typeof params === "object" && params !== null) applyThemeParams(JSON.stringify(params), root);
  };
  const chain = (earlier: Receive | undefined): Receive => (eventType, eventData) => {
    onEvent(eventType, eventData);
    earlier?.(eventType, eventData);
  };

  host.Telegram ??= {};
  host.Telegram.WebView ??= {};
  host.TelegramGameProxy ??= {};
  const webView = host.Telegram.WebView;
  const gameProxy = host.TelegramGameProxy;
  const before = {
    webView: webView.receiveEvent,
    gameProxy: gameProxy.receiveEvent,
    legacy: host.TelegramGameProxy_receiveEvent,
  };
  const ours = { webView: chain(before.webView), gameProxy: chain(before.gameProxy), legacy: chain(before.legacy) };
  webView.receiveEvent = ours.webView;
  gameProxy.receiveEvent = ours.gameProxy;
  host.TelegramGameProxy_receiveEvent = ours.legacy;

  const onMessage = (e: MessageEvent) => {
    if (window.parent === window || e.source !== window.parent || e.origin !== WEB_CLIENT_ORIGIN) return;
    if (typeof e.data !== "string") return;
    try {
      const msg = JSON.parse(e.data) as { eventType?: unknown; eventData?: unknown };
      if (typeof msg.eventType === "string") onEvent(msg.eventType, msg.eventData);
    } catch {
      // not a Telegram event
    }
  };
  window.addEventListener("message", onMessage);
  postEvent("web_app_request_theme", {});

  // Restores a receiver only while ours is still installed, so a wrapper set after ours is not dropped.
  return () => {
    window.removeEventListener("message", onMessage);
    if (webView.receiveEvent === ours.webView) webView.receiveEvent = before.webView;
    if (gameProxy.receiveEvent === ours.gameProxy) gameProxy.receiveEvent = before.gameProxy;
    if (host.TelegramGameProxy_receiveEvent === ours.legacy) host.TelegramGameProxy_receiveEvent = before.legacy;
  };
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

export type StartKind = "link" | "invite" | "approval" | "alert" | "task";

/**
 * Splits a start parameter into its kind and token: link/invite flows before sign-in, approval/alert/task deep
 * links from Telegram notifications (KIN-40, KIN-62) after it.
 */
export function startTarget(startParam: string): { kind: StartKind; token: string } | null {
  const m = /^(link|invite|approval|alert|task)_([A-Za-z0-9_-]{1,57})$/.exec(startParam);
  return m ? { kind: m[1] as StartKind, token: m[2]! } : null;
}

/** The page a notification's deep link opens after sign-in; null for the pre-sign-in flows. */
export function startPath(target: { kind: StartKind; token: string }): string | null {
  switch (target.kind) {
    case "approval":
      return `/approvals/${target.token}`;
    case "alert":
      return "/alerts";
    case "task":
      return `/tasks/${target.token}`; // task page: KIN-25
    default:
      return null;
  }
}
