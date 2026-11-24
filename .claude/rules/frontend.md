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
- Run the package's typecheck and tests for touched files before finishing.
