package restart

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	now := since.Add(75 * time.Minute)
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
		// Монитор после своего срока: уведомления об истечении нет, но Claude Code его уже снял.
		use(tsNew, "t9", "Monitor", `{"description":"короткий"}`),
		result(tsNew, "t9", "Monitor started (task m9, expires in 5m unless the source ends first)", `null`),
		// Инструмент без результата: идёт или ждёт ответа человека.
		use(tsNew, "t8", "AskUserQuestion", `{}`),
		`{"type":"api-request-blob","sessionId":"x","content":"tool_use"}`,
	)
	if want := []string{"Agent «ревью» a2", "Monitor «логи» m5"}; !slices.Equal(f.Tasks, want) {
		t.Errorf("Tasks = %q, want %q", f.Tasks, want)
	}
	if want := []string{"Agent «ревью» a2"}; !slices.Equal(f.Agents, want) {
		t.Errorf("Agents = %q, want %q", f.Agents, want)
	}
	if want := []string{"AskUserQuestion t8"}; !slices.Equal(f.Pending, want) {
		t.Errorf("Pending = %q, want %q", f.Pending, want)
	}
}

// Суб-агент кончил ход, пока его монитор жил, — «is waiting on its own background work»; монитор потом истёк молча,
// и второго уведомления родителю не пришло (sc2go 25.10). Ждёт ли он ещё — по его транскрипту.
func TestScanTranscriptWaitingAgent(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-10-24T09:00:00Z")
	sub := filepath.Join(t.TempDir(), "a1.output")
	writeLines(t, sub,
		use(tsNew, "s1", "Monitor", `{"description":"партии"}`),
		result(tsNew, "s1", "Monitor started (task ms, expires in 30m unless the source ends first)", `null`),
		notice(tsNew, "user", `<task-id>ms</task-id>\n<event>g03 exit=0</event>`),
	)
	lines := []string{
		use(tsNew, "t1", "Agent", `{"description":"части"}`),
		result(tsNew, "t1", "Async agent launched", `{"isAsync":true,"agentId":"a1"}`),
		notice(tsNew, "queue-operation", `<task-id>a1</task-id>\n<output-file>`+sub+`</output-file>\n<status>completed</status>\n<summary>Agent \"части\" is waiting on its own background work</summary>`),
	}
	if f := scanLines(t, since, mustTime(t, tsNew).Add(10*time.Minute), lines...); !slices.Equal(f.Tasks, []string{"Agent «части» a1"}) {
		t.Errorf("монитор суб-агента жив — агент ждёт: %q", f.Tasks)
	}
	if f := scanLines(t, since, mustTime(t, tsNew).Add(31*time.Minute), lines...); len(f.Tasks) != 0 {
		t.Errorf("монитор суб-агента истёк — агент кончил: %q", f.Tasks)
	}
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
	if f := scanLines(t, since, mustTime(t, tsNew).Add(31*time.Minute), lines...); len(f.Tasks) != 1 || !strings.Contains(f.Tasks[0], "транскрипт суб-агента") {
		t.Errorf("транскрипта суб-агента нет — не проверить, агент ждёт: %q", f.Tasks)
	}
}

func writeLines(t *testing.T, file string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
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

func answer(ts, sidechain string, input, cacheRead, cacheWrite, output int) string {
	return `{"type":"assistant","isSidechain":` + sidechain + `,"timestamp":"` + ts + `","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],` +
		`"usage":{"input_tokens":` + itoa(input) + `,"cache_read_input_tokens":` + itoa(cacheRead) +
		`,"cache_creation_input_tokens":` + itoa(cacheWrite) + `,"output_tokens":` + itoa(output) + `}}}`
}

func itoa(n int) string { return strconv.Itoa(n) }

// Последний запрос основной ветки: начало — реплика перед ответом, контекст — расход ответа; суб-агенты не считаются;
// запросы прошлого процесса той же беседы считаются: кэш у беседы общий.
func TestScanTranscriptContext(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-10-24T09:00:00Z")
	f := scanLines(t, since, since.Add(5*time.Hour),
		`{"type":"user","timestamp":"`+tsOld+`","message":{"content":"привет"}}`,
		answer("2026-10-24T08:00:30.000Z", "false", 3, 0, 5000, 100),
		`{"type":"user","timestamp":"2026-10-24T08:05:00.000Z","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"вывод"}]}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-10-24T08:06:00.000Z","message":{"content":"служебное"}}`,
		`{"type":"user","timestamp":"2026-10-24T08:10:00.000Z","message":{"content":[{"type":"text","text":"дальше,\n  по плану"}]}}`,
		answer("2026-10-24T08:12:00.000Z", "false", 2, 120000, 3000, 900),
		answer("2026-10-24T08:12:01.000Z", "false", 2, 120000, 3000, 1000), // тот же ответ, следующий блок
		`{"type":"user","isSidechain":true,"timestamp":"2026-10-24T08:20:00.000Z","message":{"content":"суб-агент"}}`,
		answer("2026-10-24T08:21:00.000Z", "true", 5, 0, 900000, 10),
	)
	want := contextFacts{Request: mustTime(t, "2026-10-24T08:10:00Z"), Tokens: 2 + 120000 + 3000 + 1000, CacheRead: 120000,
		Model: "claude-opus-5-5", Reply: "ok", Prompt: "дальше, по плану", PromptAt: mustTime(t, "2026-10-24T08:10:00Z")}
	if f.Context != want {
		t.Errorf("запрос: %+v, ждали %+v", f.Context, want)
	}
	marks := []RequestMark{{At: mustTime(t, "2026-10-24T08:00:00Z")}, {At: mustTime(t, "2026-10-24T08:10:00Z"), CacheRead: 120000}}
	if !slices.Equal(f.Requests, marks) {
		t.Errorf("запросы: %+v, ждали %+v", f.Requests, marks)
	}
	f = scanLines(t, since, since,
		answer(tsNew, "false", 1, 0, 200000, 10),
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-24T10:54:00.000Z","compactMetadata":{"trigger":"auto","preTokens":200011}}`,
	)
	if !f.Context.Compacted.Equal(mustTime(t, "2026-10-24T10:54:00Z")) || f.Context.Trigger != "auto" || !f.Context.Request.Equal(mustTime(t, tsNew)) {
		t.Errorf("сжатие: %+v", f.Context)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
