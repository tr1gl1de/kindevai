---
paths:
  - "web/**"
---
# UI/UX rules — the owner asked for best-practice UX (24.11.2026)

- **Accessible by default (WCAG 2.2 AA):** semantic HTML first (`<main>`, `<nav>`, `<header>`, headings in order,
  `<button>` for actions, `<a>` for navigation); every input has a visible `<label>`; ARIA only when HTML has no
  equivalent. Keyboard works everywhere, focus is always visible (`:focus-visible`), a "skip to content" link exists,
  focus moves to the page heading on route change and to the first invalid field on failed submit.
  Contrast ≥ 4.5:1 for text. Automated `axe-core` check in tests for every screen.
- **Forms:** labels above inputs, hints under them, errors next to the field (`aria-invalid`, `aria-describedby`)
  plus a summary in a `role="alert"` region; validate on submit, then live; never clear what the user typed on error;
  disable the submit button only while submitting and show progress text; correct `autocomplete`/`type`/`inputmode`.
- **Every screen has loading, empty, error and success states.** Loading uses skeletons/spinners with `aria-busy`;
  errors say what happened and what to do, with a retry; empty states explain and offer the next action.
- **Destructive actions** ask for confirmation and name the object ("Remove alice from Web?").
- **Mobile first:** works from 320 px, single column on phones, touch targets ≥ 44×44 px, no horizontal scroll,
  respects safe areas. Same UI inside the Telegram Mini App webview (theme follows `--tg-theme-*` variables).
- **Theme:** design tokens as CSS custom properties; light and dark (`prefers-color-scheme`); respect
  `prefers-reduced-motion`. No heavy UI kit; small, purposeful components in `web/src/ui/`.
- **Text:** every string via i18n (EN + RU), short and specific; buttons say what they do ("Create project", not "OK").
- **Feedback:** confirm completed actions with an unobtrusive `role="status"` message; never rely on colour alone.
