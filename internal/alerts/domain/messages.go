package domain

import (
	"fmt"
	"strings"
)

// Telegram notification texts. They are built only from the fields named here (ids, levels, numbers), never
// from free-form payload values, so nothing secret can reach a chat.

// ApprovalNotice is what a pending-approval notification says.
type ApprovalNotice struct {
	ApprovalID, TaskID, Project, Risk, Reason string
}

var severityLabel = map[Language]map[Severity]string{
	LangEN: {SeverityInfo: "Info", SeverityWarning: "Warning", SeverityCritical: "Critical"},
	LangRU: {SeverityInfo: "Инфо", SeverityWarning: "Предупреждение", SeverityCritical: "Критично"},
}

func lang(l Language) Language {
	if l == LangRU {
		return LangRU
	}
	return LangEN
}

// RenderAlert is the text of a new or escalated alert.
func RenderAlert(l Language, a Alert) string {
	l = lang(l)
	head := severityLabel[l][a.Severity]
	var body string
	switch a.Kind {
	case KindRunnerUnreachable:
		body = map[Language]string{
			LangEN: fmt.Sprintf("Runner %s is unreachable (no heartbeats).", a.Subject),
			LangRU: fmt.Sprintf("Раннер %s недоступен (нет heartbeat).", a.Subject),
		}[l]
	case KindStorageLevel:
		pct := a.Details["usedPercent"]
		body = map[Language]string{
			LangEN: fmt.Sprintf("Disk %s is %s%% full.", a.Subject, pct),
			LangRU: fmt.Sprintf("Диск %s заполнен на %s%%.", a.Subject, pct),
		}[l]
	default:
		body = map[Language]string{
			LangEN: fmt.Sprintf("New alert: %s (%s).", a.Kind, a.Subject),
			LangRU: fmt.Sprintf("Новый алерт: %s (%s).", a.Kind, a.Subject),
		}[l]
	}
	return strings.TrimSpace(head + ": " + body)
}

// RenderApproval is the text of a pending-approval notification. Deciding is possible only in the Mini App or on
// the web, after signing in there; the chat message carries no action.
func RenderApproval(l Language, n ApprovalNotice) string {
	project := n.Project
	if project == "" {
		project = "—"
	}
	if lang(l) == LangRU {
		return fmt.Sprintf("Требуется одобрение плана.\nПроект: %s\nЗадача: %s\nРиск: %s, причина: %s\nОткройте его в приложении, чтобы проверить и решить.",
			project, n.TaskID, n.Risk, n.Reason)
	}
	return fmt.Sprintf("A plan needs your approval.\nProject: %s\nTask: %s\nRisk: %s, reason: %s\nOpen it in the app to review and decide.",
		project, n.TaskID, n.Risk, n.Reason)
}

// ButtonLabel is the label of the button that opens the notification's subject in the Mini App.
func ButtonLabel(l Language, t NotificationType) string {
	ru := lang(l) == LangRU
	switch {
	case t == NotifyApprovalRequested && ru:
		return "Открыть одобрение"
	case t == NotifyApprovalRequested:
		return "Open approval"
	case ru:
		return "Открыть алерты"
	default:
		return "Open alerts"
	}
}
