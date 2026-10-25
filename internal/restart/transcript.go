package restart

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// transcriptFacts — незакрытая работа процесса по его транскрипту (~/.claude/projects/<проект>/<сессия>.jsonl),
// считая только записи с начала жизни процесса: задачи и таймеры прошлых процессов той же беседы уже мертвы.
type transcriptFacts struct {
	// Pending — вызовы инструментов без результата: инструмент идёт или ждёт ответа человека.
	Pending []string
	// Tasks — фоновые команды, мониторы и суб-агенты без уведомления о завершении.
	Tasks []string
	// Crons — живые таймеры CronCreate: живут только в процессе и пропали бы с ним.
	Crons []cronJob
	// Wakeups — ScheduleWakeup, срок которых не наступил.
	Wakeups []string
}

// cronJob — таймер сессии; поля — как в результате CronList.
type cronJob struct {
	ID        string `json:"id"`
	Prompt    string `json:"prompt"`
	Recurring bool   `json:"recurring"`
}

// object — JSON-объект с полями как есть: инструменты и версии Claude Code кладут в одно поле разные типы, и разбор
// по нужным полям не должен падать на чужих.
type object map[string]json.RawMessage

func (o object) str(key string) string {
	var s string
	if json.Unmarshal(o[key], &s) == nil {
		return s
	}
	return ""
}

func (o object) flag(key string) bool {
	var b bool
	return json.Unmarshal(o[key], &b) == nil && b
}

func (o object) seconds(key string) time.Duration {
	var n float64
	if json.Unmarshal(o[key], &n) == nil && n > 0 {
		return time.Duration(n * float64(time.Second))
	}
	return 0
}

// parseObject — объект из сырого JSON; не объект (null, строка ошибки инструмента) — пустой объект.
func parseObject(raw json.RawMessage) (object, error) {
	o := object{}
	if len(raw) == 0 || raw[0] != '{' {
		return o, nil
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("транскрипт: объект: %w", err)
	}
	return o, nil
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type toolUse struct {
	name  string
	input object
	at    time.Time
}

var (
	monitorStarted = regexp.MustCompile(`Monitor started \(task ([A-Za-z0-9_-]+)`)
	notifyTaskID   = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	notifyStatus   = regexp.MustCompile(`<status>([a-z_]+)</status>`)
)

// markers — строки без них транскрипт-скан не разбирает: в них нет ни вызовов, ни уведомлений, ни таймеров.
var markers = [][]byte{[]byte(`"tool_use"`), []byte(`"tool_result"`), []byte(`task-notification`), []byte(`scheduled_task_fire`)}

// transcriptScan — состояние разбора одного транскрипта.
type transcriptScan struct {
	since   time.Time
	uses    map[string]toolUse
	pending map[string]string
	tasks   map[string]string
	closed  map[string]bool
	crons   map[string]cronJob
	fired   map[string]bool
	wakeup  time.Time
}

// scanTranscript разбирает транскрипт процесса, начавшегося в since; незакрытое на момент now — s.facts(now).
// Последняя строка без перевода строки — недописанная запись живого файла: её разберёт следующая проверка.
func scanTranscript(r io.Reader, since time.Time) (*transcriptScan, error) {
	s := &transcriptScan{since: since, uses: map[string]toolUse{}, pending: map[string]string{}, tasks: map[string]string{},
		closed: map[string]bool{}, crons: map[string]cronJob{}, fired: map[string]bool{}}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		if err != nil {
			return nil, fmt.Errorf("транскрипт: %w", err)
		}
		if err := s.line(line); err != nil {
			return nil, err
		}
	}
}

func (s *transcriptScan) line(line []byte) error {
	if bytes.HasPrefix(line, []byte(`{"type":"api-request`)) || !slices.ContainsFunc(markers, func(m []byte) bool { return bytes.Contains(line, m) }) {
		return nil
	}
	rec, err := parseObject(line)
	if err != nil {
		return err
	}
	// Уведомление закрывает задачу, чьё бы время у записи ни стояло: закрыть задачу прошлого процесса безвредно —
	// её запуск до since и так не считается.
	if bytes.Contains(line, []byte("<task-notification>")) {
		s.notifications(string(line))
	}
	at, err := recordTime(rec)
	if err != nil {
		return err
	}
	if at.Before(s.since) {
		return nil
	}
	typ := rec.str("type")
	if typ == "system" && rec.str("subtype") == "scheduled_task_fire" {
		s.fired[rec.str("taskId")] = true
		return nil
	}
	if typ != "assistant" && typ != "user" {
		return nil
	}
	msg, err := parseObject(rec["message"])
	if err != nil {
		return err
	}
	bs, err := blocks(msg["content"])
	if err != nil {
		return err
	}
	for _, b := range bs {
		switch {
		case typ == "assistant" && b.Type == "tool_use":
			input, err := parseObject(b.Input)
			if err != nil {
				return err
			}
			s.uses[b.ID] = toolUse{name: b.Name, input: input, at: at}
			s.pending[b.ID] = b.Name
		case typ == "user" && b.Type == "tool_result":
			if err := s.result(b, rec["toolUseResult"]); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordTime — метка записи; без метки — нулевое время (запись старше любого процесса).
func recordTime(rec object) (time.Time, error) {
	ts := rec.str("timestamp")
	if ts == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("транскрипт: метка времени: %w", err)
	}
	return at, nil
}

// notifications отмечают по уведомлениям, закрыта ли задача; решает последнее. Закрывают итоговый статус
// (completed, failed, killed) и истечение монитора («[Monitor expired after 30m…]»). Не закрывают события монитора и
// «Agent … is waiting on its own background work»: статус у него completed, но суб-агент ждёт свою фоновую работу и
// вернётся с тем же task-id.
func (s *transcriptScan) notifications(text string) {
	for _, chunk := range strings.Split(text, "<task-notification>")[1:] {
		chunk, _, _ = strings.Cut(chunk, "</task-notification>")
		id := notifyTaskID.FindStringSubmatch(chunk)
		if id == nil {
			continue
		}
		status := notifyStatus.FindStringSubmatch(chunk)
		switch {
		case strings.Contains(chunk, "is waiting on its own background work"):
			s.closed[id[1]] = false
		case strings.Contains(chunk, "[Monitor expired after"):
			s.closed[id[1]] = true
		case status != nil && status[1] != "running":
			s.closed[id[1]] = true
		}
	}
}

// result — результат вызова: запуск фоновой задачи или суб-агента, снятие задачи, таймеры.
func (s *transcriptScan) result(b block, raw json.RawMessage) error {
	delete(s.pending, b.ToolUseID)
	use, known := s.uses[b.ToolUseID]
	if !known {
		return nil
	}
	res, err := parseObject(raw)
	if err != nil {
		return err
	}
	label := use.name + " «" + short(firstNonEmpty(use.input.str("description"), use.input.str("prompt"), use.input.str("command"))) + "»"
	if id := res.str("backgroundTaskId"); id != "" {
		s.tasks[id] = label
	}
	if id := res.str("agentId"); id != "" && res.flag("isAsync") {
		s.tasks[id] = label
	}
	switch use.name {
	case "Monitor":
		if m := monitorStarted.FindStringSubmatch(string(b.Content)); m != nil {
			s.tasks[m[1]] = label
		}
	case "TaskStop":
		if id := res.str("task_id"); id != "" && strings.Contains(res.str("message"), "Successfully") {
			s.closed[id] = true
		}
	case "CronCreate":
		if id := res.str("id"); id != "" {
			s.crons[id] = cronJob{ID: id, Prompt: use.input.str("prompt"), Recurring: res.flag("recurring")}
		}
	case "CronDelete":
		delete(s.crons, res.str("id"))
	case "CronList":
		return s.cronList(res)
	case "ScheduleWakeup":
		s.wakeup = time.Time{}
		if delay := use.input.seconds("delaySeconds"); delay > 0 && !use.input.flag("stop") {
			s.wakeup = use.at.Add(delay)
		}
	}
	return nil
}

// cronList — снимок таймеров целиком.
func (s *transcriptScan) cronList(res object) error {
	raw, ok := res["jobs"]
	if !ok {
		return nil
	}
	var jobs []cronJob
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return fmt.Errorf("транскрипт: CronList: %w", err)
	}
	s.crons = map[string]cronJob{}
	for _, j := range jobs {
		s.crons[j.ID] = j
	}
	return nil
}

func (s *transcriptScan) facts(now time.Time) transcriptFacts {
	var f transcriptFacts
	for id, name := range s.pending {
		f.Pending = append(f.Pending, name+" "+id)
	}
	for id, label := range s.tasks {
		if !s.closed[id] {
			f.Tasks = append(f.Tasks, label+" "+id)
		}
	}
	for id, job := range s.crons {
		// Разовый таймер, который уже сработал, Claude Code удаляет сам.
		if !job.Recurring && s.fired[id] {
			continue
		}
		f.Crons = append(f.Crons, job)
	}
	if s.wakeup.After(now) {
		f.Wakeups = append(f.Wakeups, "ScheduleWakeup до "+s.wakeup.Local().Format("15:04"))
	}
	slices.Sort(f.Pending)
	slices.Sort(f.Tasks)
	slices.SortFunc(f.Crons, func(a, b cronJob) int { return strings.Compare(a.ID, b.ID) })
	return f
}

// blocks — содержимое сообщения массивом блоков; строка (простой текст) блоков не даёт.
func blocks(raw json.RawMessage) ([]block, error) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, fmt.Errorf("транскрипт: блоки сообщения: %w", err)
	}
	return bs, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// short — подпись для журнала: первые 60 знаков.
func short(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

// scanCache — разбор транскрипта, пока файл не менялся: транскрипт лида — десятки мегабайт, а проверка раз в минуту.
type scanCache struct {
	file  string
	size  int64
	mtime time.Time
	since time.Time
	scan  *transcriptScan
}

// read — незакрытая работа по файлу транскрипта; файл тот же, размер и время те же — прошлый разбор.
func (c *scanCache) read(file string, info os.FileInfo, since, now time.Time) (transcriptFacts, error) {
	if c.scan != nil && c.file == file && c.size == info.Size() && c.mtime.Equal(info.ModTime()) && c.since.Equal(since) {
		return c.scan.facts(now), nil
	}
	f, err := os.Open(file)
	if err != nil {
		return transcriptFacts{}, fmt.Errorf("транскрипт: %w", err)
	}
	defer f.Close()
	scan, err := scanTranscript(f, since)
	if err != nil {
		return transcriptFacts{}, err
	}
	*c = scanCache{file: file, size: info.Size(), mtime: info.ModTime(), since: since, scan: scan}
	return scan.facts(now), nil
}
