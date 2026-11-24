import { useTranslation } from "react-i18next";
import { useNotify } from "./notify";

/** Copies text to the clipboard and confirms via the status region; explains the fallback when unavailable. */
export function CopyButton({ text, label, failedHint }: { text: string; label?: string; failedHint: string }) {
  const { t } = useTranslation();
  const notify = useNotify();
  return (
    <button
      type="button"
      className="button"
      onClick={() => {
        const clip = typeof navigator !== "undefined" ? navigator.clipboard : undefined;
        if (!clip) {
          notify(failedHint);
          return;
        }
        clip.writeText(text).then(
          () => notify(t("users.copied")),
          () => notify(failedHint),
        );
      }}
    >
      {label ?? t("users.copy")}
    </button>
  );
}
