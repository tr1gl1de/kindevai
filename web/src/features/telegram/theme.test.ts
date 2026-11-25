// KIN-114: the Mini App follows Telegram's theme after a reload and when the user switches it inside Telegram,
// instead of falling back to the OS scheme.
import { afterEach, describe, expect, it, vi } from "vitest";
import { followTheme, readLaunch } from "./webapp";

const INIT = "query_id=AAQ&user=%7B%22id%22%3A42%7D&auth_date=1800000000&hash=abc";
const DARK = { bg_color: "#17212b", text_color: "#f5f5f5", hint_color: "#708499", link_color: "#6ab3f3" };
const LIGHT = { bg_color: "#ffffff", text_color: "#000000", hint_color: "#707579" };

const root = document.documentElement;
const tg = (name: string) => root.style.getPropertyValue(`--tg-theme-${name}`);
const at = (hash: string) => ({ hash, pathname: "/", search: "" }) as Location;
const launchHash = (theme: object) =>
  `#tgWebAppData=${encodeURIComponent(INIT)}&tgWebAppThemeParams=${encodeURIComponent(JSON.stringify(theme))}`;

type Receiver = { receiveEvent(type: string, data: unknown): void };
const host = window as unknown as {
  Telegram?: { WebView?: Receiver };
  TelegramGameProxy?: Receiver;
  TelegramWebviewProxy?: { postEvent(type: string, data: string): void };
};

let stop: (() => void) | undefined;

afterEach(() => {
  stop?.();
  stop = undefined;
  for (const name of [...root.style].filter((p) => p.startsWith("--tg-theme-"))) root.style.removeProperty(name);
  delete root.dataset.theme;
  window.sessionStorage.clear();
  delete host.Telegram;
  delete host.TelegramGameProxy;
  delete host.TelegramWebviewProxy;
});

describe("Telegram theme", () => {
  it("applies the launch theme again after a reload", () => {
    expect(readLaunch(at(launchHash(DARK)))).not.toBeNull();
    expect(root.dataset.theme).toBe("dark");

    // A reload starts with a fresh document: Telegram's hash is gone, only session storage is left.
    root.removeAttribute("style");
    delete root.dataset.theme;
    expect(readLaunch(at(""))).not.toBeNull();
    expect(root.dataset.theme).toBe("dark");
    expect(tg("bg-color")).toBe("#17212b");
    expect(tg("link-color")).toBe("#6ab3f3");
  });

  it("asks the client for the current theme", () => {
    const postEvent = vi.fn<(type: string, data: string) => void>();
    host.TelegramWebviewProxy = { postEvent };
    stop = followTheme();
    expect(postEvent).toHaveBeenCalledWith("web_app_request_theme", "{}");
  });

  it("switches to the theme a mobile or desktop client sends with theme_changed", () => {
    readLaunch(at(launchHash(DARK)));
    stop = followTheme();

    host.Telegram!.WebView!.receiveEvent("theme_changed", { theme_params: LIGHT });
    expect(root.dataset.theme).toBe("light");
    expect(tg("bg-color")).toBe("#ffffff");
    expect(tg("hint-color")).toBe("#707579");
    // The light theme sets no link colour: the dark one must not linger.
    expect(tg("link-color")).toBe("");

    host.TelegramGameProxy!.receiveEvent("theme_changed", { theme_params: DARK });
    expect(root.dataset.theme).toBe("dark");

    // The switched theme survives a reload.
    root.removeAttribute("style");
    delete root.dataset.theme;
    readLaunch(at(""));
    expect(root.dataset.theme).toBe("dark");
    expect(tg("link-color")).toBe("#6ab3f3");
  });

  it("keeps receivers another script installed", () => {
    const earlier = vi.fn<(type: string, data: unknown) => void>();
    host.Telegram = { WebView: { receiveEvent: earlier } };
    stop = followTheme();
    host.Telegram.WebView!.receiveEvent("viewport_changed", { height: 600 });
    expect(earlier).toHaveBeenCalledWith("viewport_changed", { height: 600 });
    stop();
    stop = undefined;
    expect(host.Telegram.WebView!.receiveEvent).toBe(earlier);
  });

  it("ignores events without colour theme params and drops non-colour values", () => {
    readLaunch(at(launchHash(DARK)));
    stop = followTheme();
    const receive = host.Telegram!.WebView!.receiveEvent;
    receive("theme_changed", null);
    receive("theme_changed", { theme_params: "nope" });
    receive("popup_closed", { theme_params: LIGHT });
    expect(root.dataset.theme).toBe("dark");
    receive("theme_changed", { theme_params: { bg_color: "#ffffff", text_color: "red;}" } });
    expect(root.dataset.theme).toBe("light");
    expect(tg("text-color")).toBe("");
  });

  it("follows theme_changed from the Telegram web client only", () => {
    readLaunch(at(launchHash(DARK)));
    const parent = vi.spyOn(window, "parent", "get").mockReturnValue({ postMessage: vi.fn<() => void>() } as unknown as Window);
    try {
      stop = followTheme();
      const message = (origin: string, source: unknown) =>
        window.dispatchEvent(
          new MessageEvent("message", {
            data: JSON.stringify({ eventType: "theme_changed", eventData: { theme_params: LIGHT } }),
            origin,
            source: source as Window,
          }),
        );
      message("https://evil.example", window.parent);
      message("https://web.telegram.org", null);
      expect(root.dataset.theme).toBe("dark");
      message("https://web.telegram.org", window.parent);
      expect(root.dataset.theme).toBe("light");
    } finally {
      parent.mockRestore();
    }
  });

  it("does nothing outside Telegram", () => {
    expect(readLaunch(at(""))).toBeNull();
    expect(root.dataset.theme).toBeUndefined();
    expect([...root.style].filter((p) => p.startsWith("--tg-theme-"))).toEqual([]);
  });
});
