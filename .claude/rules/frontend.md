---
paths:
  - "web/**"
  - "**/*.ts"
  - "**/*.tsx"
---
# Frontend rules
- React + TypeScript strict mode; no `any` without a comment explaining why.
- The build output is embedded into the Go binary — keep bundles small, no heavy UI kits without approval.
- Must work inside Telegram Mini App webview and on phone widths.
- API types come from one shared module; do not hand-duplicate request/response shapes.
- Every user-visible string goes through i18next (`t("key")`): add the key to `web/src/i18n/locales/en.ts` and
  `ru.ts` in the same change; no hard-coded UI text. Test at least one screen in each locale.
- Features are organised by domain area (`web/src/features/<context>/`), mirroring the backend bounded contexts.
- Prefer established libraries (i18next, Testing Library, MSW) over custom helpers; justify new dependencies.
- Run the package's typecheck and tests for touched files before finishing.
