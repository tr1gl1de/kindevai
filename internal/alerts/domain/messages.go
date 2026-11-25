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
	Scope                                     string // execute (a plan) or integrate_dev (the merge into dev, KIN-106)
}

// ScopeIntegrateDev is the scope of an approval of the merge into dev.
const ScopeIntegrateDev = "integrate_dev"

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
	case KindRunnerOutputFull:
		mib := clip(a.Details["limitMiB"])
		body = map[Language]string{
			LangEN: fmt.Sprintf("Runner output storage is full (%s MiB): new output of running executions is dropped until the retention frees space.", mib),
			LangRU: fmt.Sprintf("Хранилище вывода раннеров заполнено (%s МиБ): новый вывод идущих исполнений отбрасывается, пока очистка не освободит место.", mib),
		}[l]
	case KindCLIHeld:
		body = cliHeld(l, a)
	default:
		body = map[Language]string{
			LangEN: fmt.Sprintf("New alert: %s (%s).", a.Kind, subject),
			LangRU: fmt.Sprintf("Новый алерт: %s (%s).", a.Kind, subject),
		}[l]
	}
	return strings.TrimSpace(head + ": " + body)
}

// cliHeld says which CLI on which runner is held, why, and what the Owner should do (KIN-83).
func cliHeld(l Language, a Alert) string {
	agent, runner, strikes := clip(a.Details["agent"]), clip(a.Details["runnerId"]), clip(a.Details["strikes"])
	parked := a.Details["parked"] == "true"
	ru := l == LangRU
	var what, do string
	switch a.Details["cause"] {
	case "auth":
		what = fmt.Sprintf("%s is logged out on runner %s", agent, runner)
		do = fmt.Sprintf("Log in to %s on runner %s, then mark it ready again on the Agents page.", agent, runner)
		if ru {
			what = fmt.Sprintf("%s на раннере %s не авторизован", agent, runner)
			do = fmt.Sprintf("Войдите в %s на раннере %s и отметьте его готовым на странице «Агенты».", agent, runner)
		}
	case "billing":
		what = fmt.Sprintf("%s on runner %s is refused by its subscription or billing", agent, runner)
		do = fmt.Sprintf("Check the %s subscription and billing used on runner %s, then mark it ready again on the Agents page.", agent, runner)
		if ru {
			what = fmt.Sprintf("%s на раннере %s: подписка или оплата отклоняет запросы", agent, runner)
			do = fmt.Sprintf("Проверьте подписку и оплату %s на раннере %s и отметьте его готовым на странице «Агенты».", agent, runner)
		}
	default:
		what = fmt.Sprintf("%s on runner %s keeps hitting its usage limit", agent, runner)
		do = "Check the plan's limits or bind another CLI to the role on the Agents page."
		if ru {
			what = fmt.Sprintf("%s на раннере %s раз за разом упирается в лимит", agent, runner)
			do = "Проверьте лимиты тарифа или назначьте роли другой CLI на странице «Агенты»."
		}
	}
	times := fmt.Sprintf(" (%s times in a row)", strikes)
	stop := " Steps go to other runners or CLIs meanwhile."
	if parked {
		stop = " Automatic retries have stopped; steps go only to other runners or CLIs."
	}
	if ru {
		times = fmt.Sprintf(" (%s раз подряд)", strikes)
		stop = " Пока шаги идут на другие раннеры или CLI."
		if parked {
			stop = " Автоматические повторы остановлены; шаги идут только на другие раннеры или CLI."
		}
	}
	return what + times + "." + stop + " " + do
}

// RenderApproval is the text of a pending-approval notification. Deciding is possible only in the Mini App or on
// the web, after signing in there; the chat message carries no action.
func RenderApproval(l Language, n ApprovalNotice) string {
	project := clip(n.Project)
	if project == "" {
		project = "—"
	}
	n.TaskID, n.Risk, n.Reason = clip(n.TaskID), clip(n.Risk), clip(n.Reason)
	if n.Scope == ScopeIntegrateDev {
		if lang(l) == LangRU {
			return fmt.Sprintf("Проверенная работа ждёт одобрения на мерж в dev.\nПроект: %s\nЗадача: %s\nРиск: %s\nОткройте его в приложении, чтобы проверить и решить.",
				project, n.TaskID, n.Risk)
		}
		return fmt.Sprintf("Verified work waits for your approval to merge into dev.\nProject: %s\nTask: %s\nRisk: %s\nOpen it in the app to review and decide.",
			project, n.TaskID, n.Risk)
	}
	if lang(l) == LangRU {
		return fmt.Sprintf("Требуется одобрение плана.\nПроект: %s\nЗадача: %s\nРиск: %s, причина: %s\nОткройте его в приложении, чтобы проверить и решить.",
			project, n.TaskID, n.Risk, n.Reason)
	}
	return fmt.Sprintf("A plan needs your approval.\nProject: %s\nTask: %s\nRisk: %s, reason: %s\nOpen it in the app to review and decide.",
		project, n.TaskID, n.Risk, n.Reason)
}

// TaskNotice is what a "task finished" notification says (KIN-62).
type TaskNotice struct {
	TaskID, Title, Project string
	Failed                 bool // FAILED, else COMPLETED
}

// RenderTask is the text of a finished task's notification. The failure reason is not included (it may quote
// CLI output); the task page shows it.
func RenderTask(l Language, n TaskNotice) string {
	title, project := clip(n.Title), clip(n.Project)
	if title == "" {
		title = clip(n.TaskID)
	}
	if project == "" {
		project = "—"
	}
	switch {
	case lang(l) == LangRU && n.Failed:
		return fmt.Sprintf("Задача завершилась с ошибкой: %s\nПроект: %s\nОткройте её в приложении, чтобы узнать причину.", title, project)
	case lang(l) == LangRU:
		return fmt.Sprintf("Задача выполнена: %s\nПроект: %s\nОткройте её в приложении, чтобы посмотреть результат.", title, project)
	case n.Failed:
		return fmt.Sprintf("Task failed: %s\nProject: %s\nOpen it in the app to see why.", title, project)
	default:
		return fmt.Sprintf("Task completed: %s\nProject: %s\nOpen it in the app to see the result.", title, project)
	}
}

// ButtonLabel is the label of the button that opens the notification's subject in the Mini App.
func ButtonLabel(l Language, t NotificationType) string {
	ru := lang(l) == LangRU
	switch {
	case t == NotifyApprovalRequested && ru:
		return "Открыть одобрение"
	case t == NotifyApprovalRequested:
		return "Open approval"
	case t == NotifyTaskFinished && ru:
		return "Открыть задачу"
	case t == NotifyTaskFinished:
		return "Open task"
	case ru:
		return "Открыть алерты"
	default:
		return "Open alerts"
	}
}
