package domain

import (
	"fmt"
	"strings"
	"unicode"
)

// Telegram notification texts. They are built only from the fields named here (ids, levels, numbers, short
// codes), each clipped and stripped of control characters, so a message stays far below Telegram's 4096-character
// limit and no other payload value can reach a chat.

// maxField is the most runes a single interpolated value contributes to a message.
const maxField = 120

// clip makes an interpolated value safe for a chat message: no control characters, at most maxField runes.
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxField {
		s = string(r[:maxField-1]) + "…"
	}
	return s
}

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
	subject := clip(a.Subject)
	var body string
	switch a.Kind {
	case KindRunnerUnreachable:
		body = map[Language]string{
			LangEN: fmt.Sprintf("Runner %s is unreachable (no heartbeats).", subject),
			LangRU: fmt.Sprintf("Раннер %s недоступен (нет heartbeat).", subject),
		}[l]
	case KindStorageLevel:
		pct := clip(a.Details["usedPercent"])
		body = map[Language]string{
			LangEN: fmt.Sprintf("Disk %s is %s%% full.", a.Subject, pct),
			LangRU: fmt.Sprintf("Диск %s заполнен на %s%%.", a.Subject, pct),
		}[l]
	case KindEgressCheckFailed:
		code := clip(a.Details["code"])
		body = map[Language]string{
			LangEN: fmt.Sprintf("Runner %s: egress check failed (%s).", subject, code),
			LangRU: fmt.Sprintf("Раннер %s: проверка egress не прошла (%s).", subject, code),
		}[l]
	case KindCIFailed:
		c := clip(a.Details["conclusion"])
		body = map[Language]string{
			LangEN: fmt.Sprintf("CI failed on %s (%s).", subject, c),
			LangRU: fmt.Sprintf("CI упал на %s (%s).", subject, c),
		}[l]
	case KindIntegrationCircuitOpen:
		body = map[Language]string{
			LangEN: fmt.Sprintf("Integration %s is failing: calls suspended (circuit open).", subject),
			LangRU: fmt.Sprintf("Интеграция %s не отвечает: вызовы приостановлены (circuit open).", subject),
		}[l]
	case KindTaskBlocked:
		r := clip(a.Details["reason"])
		body = map[Language]string{
			LangEN: fmt.Sprintf("Task %s is blocked (%s).", subject, r),
			LangRU: fmt.Sprintf("Задача %s заблокирована (%s).", subject, r),
		}[l]
	default:
		body = map[Language]string{
			LangEN: fmt.Sprintf("New alert: %s (%s).", a.Kind, subject),
			LangRU: fmt.Sprintf("Новый алерт: %s (%s).", a.Kind, subject),
		}[l]
	}
	return strings.TrimSpace(head + ": " + body)
}

// RenderApproval is the text of a pending-approval notification. Deciding is possible only in the Mini App or on
// the web, after signing in there; the chat message carries no action.
func RenderApproval(l Language, n ApprovalNotice) string {
	project := clip(n.Project)
	if project == "" {
		project = "—"
	}
	n.TaskID, n.Risk, n.Reason = clip(n.TaskID), clip(n.Risk), clip(n.Reason)
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
