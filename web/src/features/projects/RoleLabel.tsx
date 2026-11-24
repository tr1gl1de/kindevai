import { useTranslation } from "react-i18next";
import type { Project } from "../../api/types";

export function RoleLabel({ role }: { role: Project["myRole"] }) {
  const { t } = useTranslation();
  return <span className={`badge role-${role}`}>{t(`projects.role_${role}`)}</span>;
}
