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
    case "unauthenticated":
      return t("errors.unauthenticated");
    case "cross_origin":
      return t("errors.cross_origin");
    case "not_found":
      return t("errors.not_found");
    case "validation": // a validation error not tied to a form field (e.g. from a row action)
      return validationMessage(t, e.detail) ?? t("errors.unknown");
    case "invitation_invalid":
      return t("invite.invitation_invalid");
    case "invitation_used":
      return t("invite.invitation_used");
    case "invitation_revoked":
      return t("invite.invitation_revoked");
    case "invitation_expired":
      return t("invite.invitation_expired");
    case "invitation_not_found":
      return t("errors.not_found");
    case "invalid_transition":
      return t("runners.invalid_transition");
    case "telegram_not_configured":
    case "telegram_auth_invalid":
    case "telegram_auth_expired":
    case "telegram_not_linked":
    case "account_disabled":
    case "telegram_taken":
    case "telegram_already_linked":
    case "link_invalid":
    case "link_expired":
    case "link_used":
    case "link_cancelled":
    case "link_claimed":
    case "link_not_confirmed":
      return t(`telegram.err_${e.code}`);
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
  "password_reused",
  "owner_cannot_be_disabled",
  "owner_cannot_be_reset",
  "project_not_found",
  "ttl_out_of_range",
  "invitation_not_pending",
  "projects_required",
  "token_not_pending",
]);

/** Message for a server validation code, or undefined if unknown. */
export function validationMessage(t: TFunction, code: string | undefined): string | undefined {
  if (!code || !validationKeys.has(code)) return undefined;
  return t(`validation.${code}` as "validation.login_format");
}
