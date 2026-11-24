import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { fetchHealth } from "./api/client";
import type { Health } from "./api/types";
import { LanguageSwitcher } from "./i18n/LanguageSwitcher";

type State = { kind: "loading" } | { kind: "ok"; health: Health } | { kind: "error" };

export function App() {
  const { t } = useTranslation();
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    const ctrl = new AbortController();
    fetchHealth(ctrl.signal)
      .then((health) => setState({ kind: "ok", health }))
      .catch(() => {
        if (!ctrl.signal.aborted) setState({ kind: "error" });
      });
    return () => ctrl.abort();
  }, []);

  return (
    <main className="app">
      <header className="app-header">
        <h1>{t("app.title")}</h1>
        <LanguageSwitcher />
      </header>
      <p role="status">
        {state.kind === "loading" && t("health.loading")}
        {state.kind === "ok" && t("health.online", { version: state.health.version })}
        {state.kind === "error" && t("health.unreachable")}
      </p>
    </main>
  );
}
