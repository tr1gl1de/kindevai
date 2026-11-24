// KIN-66: lint every stylesheet so a syntax error (e.g. a brace lost in a merge) fails `npm run lint`, `make lint`
// and CI. The recommended config only flags likely mistakes, not formatting.
/** @type {import("stylelint").Config} */
export default {
  extends: ["stylelint-config-recommended"],
  rules: {
    // Order-based and noisy in one feature-ordered stylesheet; it flags unrelated selectors (".secret h3:focus" vs
    // ".card-list h3") and would force reshuffling rules that parallel tasks edit.
    "no-descending-specificity": null,
  },
};
