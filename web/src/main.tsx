import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { App } from "./App";
import { createI18n } from "./i18n";
import "./styles.css";

const root = document.getElementById("root");
if (!root) throw new Error("#root element missing");
const i18n = await createI18n();
createRoot(root).render(
  <StrictMode>
    <I18nextProvider i18n={i18n}>
      <App />
    </I18nextProvider>
  </StrictMode>,
);
