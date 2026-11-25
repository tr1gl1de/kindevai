// The base stylesheet comes first: feature stylesheets (imported by their components) follow it in the cascade.
import "./styles.css";
import { QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { BrowserRouter } from "react-router";
import { App } from "./app/App";
import { createQueryClient } from "./app/queryClient";
import { createI18n, LOCALE_STORAGE_KEY } from "./i18n";
import { readLaunch, seedLocale, signalReady } from "./features/telegram/webapp";

const root = document.getElementById("root");
if (!root) throw new Error("#root element missing");
const telegram = readLaunch();
seedLocale(telegram, LOCALE_STORAGE_KEY);
const i18n = await createI18n();
const queryClient = createQueryClient({
  defaultOptions: { queries: { staleTime: 30_000, refetchOnWindowFocus: true, retry: 1 } },
});
createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <I18nextProvider i18n={i18n}>
        <BrowserRouter>
          <App telegram={telegram} />
        </BrowserRouter>
      </I18nextProvider>
    </QueryClientProvider>
  </StrictMode>,
);
if (telegram) signalReady();
