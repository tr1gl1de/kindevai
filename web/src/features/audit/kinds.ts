import { en } from "../../i18n/locales/en";

/** Key of a kind under audit.kinds: "<namespace>.<action>" becomes "<namespace>_<action>". */
export function kindKey(kind: string): string {
  return kind.replace(".", "_");
}

/** Whether a kind has a translated name; the RU locale has the same keys (its type is Messages). */
export function isKnownKind(kind: string): kind is `${string}.${string}` {
  return Object.hasOwn(en.audit.kinds, kindKey(kind));
}
