import type { TFunction } from "i18next";
import { isApiError } from "../api/client";

/** Human message for a failed request that is not tied to a field. */
export function errorMessage(t: TFunction, e: unknown): string {
  if (!isApiError(e)) return t("errors.unknown");
  switch (e.code) {
    case "network":
      return t("errors.network");
    case "rate_limited":
      return t("errors.rate_limited");
    case "forbidden":
      return t("errors.forbidden");
    case "internal":
      return t("errors.internal");
    default:
      return t("errors.unknown");
  }
}

const validationKeys = new Set([
  "login_format",
  "password_too_short",
  "password_too_long",
  "password_equals_login",
  "platform_name_format",
  "name_format",
  "slug_format",
  "role_unknown",
  "user_not_found",
  "owner_has_full_access",
  "user_disabled",
  "slug_taken",
]);

/** Message for a server validation code, or undefined if unknown. */
export function validationMessage(t: TFunction, code: string | undefined): string | undefined {
  if (!code || !validationKeys.has(code)) return undefined;
  return t(`validation.${code}` as "validation.login_format");
}
