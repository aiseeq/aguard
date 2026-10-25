package restart

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Строки транскрипта в том виде, в каком их пишет Claude Code 2.1.309–2.1.310 (сокращённо).
const (
	tsOld = "2026-10-24T08:00:00.000Z"
	tsNew = "2026-10-24T10:00:00.000Z"
)

func use(ts, id, name, input string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}`
}

func result(ts, id, content, toolUseResult string) string {
	return `{"type":"user","timestamp":"` + ts + `","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` + content + `"}]},"toolUseResult":` + toolUseResult + `}`
}

func notice(ts, typ, body string) string {
	return `{"type":"` + typ + `","timestamp":"` + ts + `","content":"<task-notification>\n` + body + `\n</task-notification>"}`
}

func scanLines(t *testing.T, since, now time.Time, lines ...string) transcriptFacts {
	t.Helper()
	s, err := scanTranscript(strings.NewReader(strings.Join(lines, "\n")+"\n"), since)
	if err != nil {
		t.Fatal(err)
	}
	return s.facts(now)
}

func TestScanTranscriptTasks(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-10-24T09:00:00Z")
	now := since.Add(5 * time.Hour)
	f := scanLines(t, since, now,
		// Запуск прошлого процесса не считается: его задачи умерли с ним.
		use(tsOld, "t0", "Bash", `{"command":"sleep 1000","run_in_background":true}`),
		result(tsOld, "t0", "Command running in background with ID: bold", `{"backgroundTaskId":"bold"}`),
		// Фоновая команда, закрытая уведомлением.
		use(tsNew, "t1", "Bash", `{"command":"make","description":"сборка","run_in_background":true}`),
		result(tsNew, "t1", "Command running in background with ID: b1", `{"backgroundTaskId":"b1"}`),
		notice(tsNew, "user", `<task-id>b1</task-id>\n<status>completed</status>`),
		// Суб-агент, который ждёт свою фоновую работу: статус completed, но он не закончил.
		use(tsNew, "t2", "Agent", `{"description":"ревью"}`),
		result(tsNew, "t2", "Async agent launched", `{"isAsync":true,"agentId":"a2"}`),
		notice(tsNew, "queue-operation", `<task-id>a2</task-id>\n<status>completed</status>\n<summary>Agent \"ревью\" is waiting on its own background work</summary>`),
		// Суб-агент, вернувшийся после ожидания.
		use(tsNew, "t3", "Agent", `{"description":"поиск"}`),
		result(tsNew, "t3", "Async agent launched", `{"isAsync":true,"agentId":"a3"}`),
		notice(tsNew, "user", `<task-id>a3</task-id>\n<status>completed</status>\n<summary>Agent \"поиск\" is waiting on its own background work</summary>`),
		notice(tsNew, "user", `<task-id>a3</task-id>\n<status>completed</status>\n<summary>Agent \"поиск\" finished</summary>`),
		// Монитор: событие не закрывает, истечение закрывает; второй монитор снят TaskStop.
		use(tsNew, "t4", "Monitor", `{"description":"полосы"}`),
		result(tsNew, "t4", "Monitor started (task m4, expires in 30m)", `null`),
		notice(tsNew, "user", `<task-id>m4</task-id>\n<event>[Monitor expired after 30m with 5 events delivered.]</event>`),
		use(tsNew, "t5", "Monitor", `{"description":"логи"}`),
		result(tsNew, "t5", "Monitor started (task m5, expires in 30m)", `null`),
		notice(tsNew, "user", `<task-id>m5</task-id>\n<event>строка лога</event>`),
		use(tsNew, "t6", "Monitor", `{"description":"журнал"}`),
		result(tsNew, "t6", "Monitor started (task m6, expires in 30m)", `null`),
		use(tsNew, "t7", "TaskStop", `{"task_id":"m6"}`),
		result(tsNew, "t7", "ok", `{"message":"Successfully stopped task: m6","task_id":"m6"}`),
		// Инструмент без результата: идёт или ждёт ответа человека.
		use(tsNew, "t8", "AskUserQuestion", `{}`),
		`{"type":"api-request-blob","sessionId":"x","content":"tool_use"}`,
	)
	if want := []string{"Agent «ревью» a2", "Monitor «логи» m5"}; !slices.Equal(f.Tasks, want) {
		t.Errorf("Tasks = %q, want %q", f.Tasks, want)
	}
	if want := []string{"AskUserQuestion t8"}; !slices.Equal(f.Pending, want) {
		t.Errorf("Pending = %q, want %q", f.Pending, want)
	}
}

func TestScanTranscriptCrons(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-10-24T09:00:00Z")
	now := since.Add(2 * time.Hour)
	f := scanLines(t, since, now,
		use(tsNew, "c1", "CronCreate", `{"cron":"47 1 * * *","prompt":"Сон (скилл sleep)","recurring":true}`),
		result(tsNew, "c1", "ok", `{"id":"j1","recurring":true}`),
		use(tsNew, "c2", "CronCreate", `{"cron":"13 21 24 10 *","prompt":"замер","recurring":false}`),
		result(tsNew, "c2", "ok", `{"id":"j2","recurring":false}`),
		use(tsNew, "c3", "CronCreate", `{"cron":"5 10 24 10 *","prompt":"разовый","recurring":false}`),
		result(tsNew, "c3", "ok", `{"id":"j3","recurring":false}`),
		`{"type":"system","subtype":"scheduled_task_fire","timestamp":"`+tsNew+`","taskId":"j3"}`,
		use(tsNew, "c4", "CronCreate", `{"cron":"0 * * * *","prompt":"удалённый","recurring":true}`),
		result(tsNew, "c4", "ok", `{"id":"j4","recurring":true}`),
		use(tsNew, "c5", "CronDelete", `{"id":"j4"}`),
		result(tsNew, "c5", "ok", `{"id":"j4"}`),
		use(tsNew, "w1", "ScheduleWakeup", `{"delaySeconds":1200}`),
		result(tsNew, "w1", "ok", `null`),
	)
	ids := make([]string, 0, len(f.Crons))
	for _, c := range f.Crons {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"j1", "j2"}) {
		t.Errorf("Crons = %q, want j1 j2", ids)
	}
	if len(f.Wakeups) != 0 {
		t.Errorf("будильник через 20 минут после 10:00 к 11:00 сработал: %q", f.Wakeups)
	}

	// CronList — снимок списка целиком; будильник в будущем держит.
	f = scanLines(t, since, since.Add(time.Hour+5*time.Minute),
		use(tsNew, "c1", "CronCreate", `{"prompt":"Сон (скилл sleep)","recurring":true}`),
		result(tsNew, "c1", "ok", `{"id":"j1","recurring":true}`),
		use(tsNew, "l1", "CronList", `{}`),
		result(tsNew, "l1", "ok", `{"jobs":[{"id":"j9","prompt":"Дайджест (скилл digest)","recurring":true}]}`),
		use(tsNew, "w1", "ScheduleWakeup", `{"delaySeconds":1200}`),
		result(tsNew, "w1", "ok", `null`),
	)
	if len(f.Crons) != 1 || f.Crons[0].ID != "j9" {
		t.Errorf("CronList: %+v", f.Crons)
	}
	if len(f.Wakeups) != 1 {
		t.Errorf("будильник до 10:20 при now 10:05 должен держать: %q", f.Wakeups)
	}
}

func TestScanTranscriptBrokenLine(t *testing.T) {
	// Недописанная последняя строка живого файла — не ошибка; битая целая строка — ошибка (сомнение).
	if _, err := scanTranscript(strings.NewReader(`{"type":"user","message":{"content":[{"type":"tool_result"`), time.Time{}); err != nil {
		t.Errorf("недописанная строка: %v", err)
	}
	if _, err := scanTranscript(strings.NewReader(`{"type":"user","tool_result":x}`+"\n"), time.Time{}); err == nil {
		t.Error("битая строка должна быть ошибкой")
	}
}
