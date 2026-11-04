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

	"github.com/aiseeq/akit/transcript/waits"
)

// transcriptFacts — незакрытая работа процесса по его транскрипту (~/.claude/projects/<проект>/<сессия>.jsonl),
// считая только записи с начала жизни процесса: задачи и таймеры прошлых процессов той же беседы уже мертвы.
type transcriptFacts struct {
	// Pending — вызовы инструментов без результата: инструмент идёт или ждёт ответа человека.
	Pending      []string
	PendingWaits int
	// Tasks — фоновые команды, мониторы и суб-агенты без уведомления о завершении; Agents — суб-агенты и workflow
	// среди них: держат и сброс набором /clear.
	Tasks  []string
	Agents []string
	// Crons — живые таймеры CronCreate: живут только в процессе и пропали бы с ним.
	Crons []cronJob
	// Wakeups — ScheduleWakeup, срок которых не наступил.
	Wakeups []string
	// Context — последний запрос основной ветки, с любого процесса беседы: кэш подсказки переживает перезапуск.
	Context contextFacts
	// Requests — последние запросы основной ветки: исход решения о сжатии смотрит первый запрос после решения.
	Requests []RequestMark
}

// RequestMark — запрос основной ветки: начало и сколько он прочитал из кэша.
type RequestMark struct {
	At        time.Time `json:"at"`
	CacheRead int64     `json:"cache_read"`
}

// requestsKept — сколько последних запросов помнить: проверка раз в минуту, а за минуту запросов меньше.
const requestsKept = 64

// contextFacts — последний запрос основной ветки беседы. Кэш подсказки живёт час от начала запроса, который его
// прочитал или записал.
type contextFacts struct {
	// Request — начало последнего запроса: запись, после которой он ушёл (реплика или результат инструмента).
	Request time.Time
	// Tokens — контекст после ответа: вход, кэш и выход; столько уйдёт в следующий запрос.
	Tokens int64
	// CacheRead — сколько ответ прочитал из кэша: при большом контексте 0 — кэш к запросу умер.
	CacheRead int64
	// Compacted — последнее сжатие (compact_boundary), Trigger — чьё: auto, manual.
	Compacted time.Time
	Trigger   string
	// Model — модель последнего ответа: по ней цена кэша.
	Model string
	// Reply — конец последнего текста ответа (итог хода), Prompt — начало последней реплики человека и её время.
	Reply    string
	Prompt   string
	PromptAt time.Time
	// Human — последняя реплика человека из терминала (promptSource, origin.kind=human): typed — отправлена Enter в
	// простое, запись в момент отправки; queued — набрана во время хода, запись позже Enter. HumanAt — её время.
	Human   string
	HumanAt time.Time
}

// textLimit — сколько знаков текста ответа и реплики держать для журнала.
const textLimit = 300

// cronJob — таймер сессии; поля — как в результате CronList.
type cronJob struct {
	ID        string `json:"id"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	Recurring bool   `json:"recurring"`
	Own       bool   `json:"-"`
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

func (o object) num(key string) int64 {
	var n int64
	if json.Unmarshal(o[key], &n) == nil {
		return n
	}
	return 0
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
	Text      string          `json:"text"`
}

type toolUse struct {
	name  string
	input object
	at    time.Time
	wait  bool
}

var (
	monitorStarted = regexp.MustCompile(`Monitor started \(task ([A-Za-z0-9_-]+)(?:, expires in (\d+[smh]))?`)
	notifyTaskID   = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	notifyOutput   = regexp.MustCompile(`<output-file>([^<]+)</output-file>`)
	notifyStatus   = regexp.MustCompile(`<status>([a-z_]+)</status>`)
)

// markers — строки без них транскрипт-скан не разбирает: в них нет ни вызовов, ни уведомлений, ни таймеров, ни
// запросов и сжатий.
var markers = [][]byte{[]byte(`"tool_use"`), []byte(`"tool_result"`), []byte(`task-notification`), []byte(`scheduled_task_fire`),
	[]byte(`"type":"user"`), []byte(`"usage"`), []byte(`compact_boundary`), []byte(`session_cron_carry`)}

// transcriptScan — состояние разбора одного транскрипта.
type transcriptScan struct {
	since   time.Time
	uses    map[string]toolUse
	pending map[string]string
	tasks   map[string]string
	agents  map[string]bool // суб-агенты и workflow среди tasks
	closed  map[string]bool
	// expires — срок монитора: после него Claude Code монитор снимает, а уведомление об этом приходит не всегда.
	expires map[string]time.Time
	// waiting — суб-агент, который ждёт свою фоновую работу, и его транскрипт; subs — разбор этих транскриптов.
	waiting  map[string]string
	subs     map[string]*scanCache
	crons    map[string]cronJob
	fired    map[string]bool
	wakeup   time.Time
	userAt   time.Time // последняя реплика или результат инструмента основной ветки: после неё уходит запрос
	context  contextFacts
	requests []RequestMark
}

// scanTranscript разбирает транскрипт процесса, начавшегося в since; незакрытое на момент now — s.facts(now).
// Последняя строка без перевода строки — недописанная запись живого файла: её разберёт следующая проверка.
func scanTranscript(r io.Reader, since time.Time) (*transcriptScan, error) {
	s := newScan(since)
	if _, err := s.feed(r); err != nil {
		return nil, err
	}
	return s, nil
}

func newScan(since time.Time) *transcriptScan {
	return &transcriptScan{since: since, uses: map[string]toolUse{}, pending: map[string]string{}, tasks: map[string]string{},
		agents: map[string]bool{}, closed: map[string]bool{}, expires: map[string]time.Time{}, waiting: map[string]string{}, subs: map[string]*scanCache{},
		crons: map[string]cronJob{}, fired: map[string]bool{}}
}

// feed разбирает строки до конца r; n — сколько байт целых строк разобрано: с этого места продолжит следующий feed.
// После ошибки разбор неполон, его не продолжать.
func (s *transcriptScan) feed(r io.Reader) (n int64, err error) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("транскрипт: %w", err)
		}
		if err := s.line(line); err != nil {
			return n, err
		}
		n += int64(len(line))
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
	typ := rec.str("type")
	if typ == "attachment" {
		return s.cronCarry(rec)
	}
	if !rec.flag("isSidechain") {
		if err := s.request(rec, at); err != nil {
			return err
		}
	}
	if at.Before(s.since) {
		return nil
	}
	if typ == "system" && rec.str("subtype") == "scheduled_task_fire" {
		s.fired[rec.str("taskId")] = true
		return nil
	}
	if typ != "assistant" && typ != "user" {
		return nil
	}
	return s.calls(typ, rec, at)
}

// calls — вызовы инструментов и их результаты в записи typ (assistant или user).
func (s *transcriptScan) calls(typ string, rec object, at time.Time) error {
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
			cmd := input.str("command")
			s.uses[b.ID] = toolUse{name: b.Name, input: input, at: at, wait: b.Name == "Bash" && waits.CommandHow(cmd) != ""}
			s.pending[b.ID] = b.Name
		case typ == "user" && b.Type == "tool_result":
			if err := s.result(b, rec["toolUseResult"]); err != nil {
				return err
			}
		}
	}
	return nil
}

// request — запросы и сжатия основной ветки. Записей ответа у запроса несколько (по блоку), расход — в каждой.
func (s *transcriptScan) request(rec object, at time.Time) error {
	switch rec.str("type") {
	case "user":
		s.userAt = at
		return s.prompt(rec, at)
	case "system":
		if rec.str("subtype") != "compact_boundary" {
			return nil
		}
		meta, err := parseObject(rec["compactMetadata"])
		if err != nil {
			return err
		}
		s.context.Compacted, s.context.Trigger = at, meta.str("trigger")
	case "assistant":
		msg, err := parseObject(rec["message"])
		if err != nil {
			return err
		}
		u, err := parseObject(msg["usage"])
		if err != nil || len(u) == 0 {
			return err
		}
		s.context.Request = s.userAt
		if s.userAt.IsZero() || s.userAt.After(at) {
			s.context.Request = at
		}
		s.context.CacheRead = u.num("cache_read_input_tokens")
		if n := len(s.requests); n == 0 || !s.requests[n-1].At.Equal(s.context.Request) {
			if n >= 2*requestsKept {
				s.requests = slices.Clone(s.requests[n-requestsKept:])
			}
			s.requests = append(s.requests, RequestMark{At: s.context.Request})
		}
		s.requests[len(s.requests)-1].CacheRead = s.context.CacheRead
		s.context.Tokens = u.num("input_tokens") + s.context.CacheRead + u.num("cache_creation_input_tokens") + u.num("output_tokens")
		s.context.Model = msg.str("model")
		bs, err := blocks(msg["content"])
		if err != nil {
			return err
		}
		for _, b := range bs {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				s.context.Reply = tail(b.Text, textLimit)
			}
		}
	}
	return nil
}

// prompt — реплика человека (не результат инструмента и не служебная запись): начало текста и время.
func (s *transcriptScan) prompt(rec object, at time.Time) error {
	if rec.flag("isMeta") {
		return nil
	}
	msg, err := parseObject(rec["message"])
	if err != nil {
		return err
	}
	var text strings.Builder
	text.WriteString(msg.str("content"))
	bs, err := blocks(msg["content"])
	if err != nil {
		return err
	}
	for _, b := range bs {
		if b.Type == "tool_result" {
			return nil
		}
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(text.String()) != "" {
		s.context.Prompt, s.context.PromptAt = head(text.String(), textLimit), at
	}
	origin, err := parseObject(rec["origin"])
	if err != nil {
		return err
	}
	if src := rec.str("promptSource"); origin.str("kind") == "human" && src != "" {
		s.context.Human, s.context.HumanAt = src, at
	}
	return nil
}

// head и tail — первые и последние n знаков текста одной строкой.
func head(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func tail(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return string(r)
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
			if out := notifyOutput.FindStringSubmatch(chunk); out != nil {
				s.waiting[id[1]] = out[1]
			}
		case strings.Contains(chunk, "[Monitor expired after"):
			s.closed[id[1]] = true
			delete(s.waiting, id[1])
		case status != nil && status[1] != "running":
			s.closed[id[1]] = true
			delete(s.waiting, id[1])
		}
	}
}

// result — результат вызова: запуск фоновой задачи или суб-агента, снятие задачи, таймеры.
func (s *transcriptScan) result(b block, raw json.RawMessage) error {
	delete(s.pending, b.ToolUseID)
	use, known := s.uses[b.ToolUseID]
	delete(s.uses, b.ToolUseID) // ввод вызова нужен только для его результата: транскрипт лида — тысячи вызовов
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
		s.agents[id] = use.name == "Workflow"
	}
	if id := res.str("agentId"); id != "" && res.flag("isAsync") {
		s.tasks[id], s.agents[id] = label, true
	}
	switch use.name {
	case "Monitor":
		if m := monitorStarted.FindStringSubmatch(string(b.Content)); m != nil {
			s.tasks[m[1]] = label
			if ttl, err := time.ParseDuration(m[2]); err == nil {
				s.expires[m[1]] = use.at.Add(ttl)
			}
		}
	case "TaskStop":
		if id := res.str("task_id"); id != "" && strings.Contains(res.str("message"), "Successfully") {
			s.closed[id] = true
		}
	case "CronCreate":
		if id := res.str("id"); id != "" {
			s.crons[id] = cronJob{ID: id, Cron: use.input.str("cron"), Prompt: use.input.str("prompt"), Recurring: res.flag("recurring"), Own: true}
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
	previous := s.crons
	s.crons = map[string]cronJob{}
	for _, j := range jobs {
		if old, ok := previous[j.ID]; ok {
			j.Cron = firstNonEmpty(j.Cron, old.Cron)
			j.Own = old.Own
		}
		s.crons[j.ID] = j
	}
	return nil
}

// cronCarry — снимок таймеров на старте беседы или после сжатия: свои и унаследованные из прежней беседы.
func (s *transcriptScan) cronCarry(rec object) error {
	attachment, err := parseObject(rec["attachment"])
	if err != nil {
		return err
	}
	if attachment.str("type") != "session_cron_carry" {
		return nil
	}
	s.crons = map[string]cronJob{}
	for _, group := range []struct {
		field string
		own   bool
	}{{"tasks", true}, {"otherArmed", false}} {
		raw := attachment[group.field]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var jobs []cronJob
		if err := json.Unmarshal(raw, &jobs); err != nil {
			return fmt.Errorf("транскрипт: session_cron_carry %s: %w", group.field, err)
		}
		for _, job := range jobs {
			if job.ID == "" {
				continue
			}
			job.Own = group.own
			s.crons[job.ID] = job
		}
	}
	return nil
}

func (s *transcriptScan) facts(now time.Time) transcriptFacts {
	var f transcriptFacts
	for id, name := range s.pending {
		f.Pending = append(f.Pending, name+" "+id)
		if s.uses[id].wait {
			f.PendingWaits++
		}
	}
	for id, label := range s.tasks {
		if s.closed[id] {
			continue
		}
		switch done, err := s.ended(id, now); {
		case err != nil:
			f.Tasks = append(f.Tasks, label+" "+id+" ("+err.Error()+")")
		case !done:
			f.Tasks = append(f.Tasks, label+" "+id)
		default:
			continue
		}
		if s.agents[id] {
			f.Agents = append(f.Agents, label+" "+id)
		}
	}
	f.Requests = slices.Clone(s.requests[max(0, len(s.requests)-requestsKept):])
	for id, job := range s.crons {
		// Разовый таймер, который уже сработал, Claude Code удаляет сам.
		if !job.Recurring && s.fired[id] {
			continue
		}
		f.Crons = append(f.Crons, job)
	}
	f.Context = s.context
	if s.wakeup.After(now) {
		f.Wakeups = append(f.Wakeups, "ScheduleWakeup до "+s.wakeup.Local().Format("15:04"))
	}
	slices.Sort(f.Pending)
	slices.Sort(f.Tasks)
	slices.Sort(f.Agents)
	slices.SortFunc(f.Crons, func(a, b cronJob) int { return strings.Compare(a.ID, b.ID) })
	return f
}

// ended — задача без уведомления о конце, но кончилась: монитор после своего срока или суб-агент, чья фоновая
// работа закончилась. Монитор суб-агента истекает после конца хода молча, и родителю уведомление не приходит
// (sc2go 25.10: агент a0505c3725309a1d5 держал довод tasks 29 часов). Транскрипта суб-агента нет или он не
// разбирается — не проверить: задача остаётся, ошибка — в её подписи.
func (s *transcriptScan) ended(id string, now time.Time) (bool, error) {
	if exp, ok := s.expires[id]; ok && !now.Before(exp) {
		return true, nil
	}
	file, ok := s.waiting[id]
	if !ok {
		return false, nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return false, fmt.Errorf("транскрипт суб-агента: %w", err)
	}
	c := s.subs[file]
	if c == nil {
		c = &scanCache{}
		s.subs[file] = c
	}
	sub, err := c.read(file, info, time.Time{}, now)
	if err != nil {
		return false, fmt.Errorf("суб-агент: %w", err)
	}
	return len(sub.Tasks) == 0, nil
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

// scanCache — разбор транскрипта между проверками: транскрипт лида — десятки мегабайт, а проверка раз в минуту.
// Файл не менялся — прошлый разбор; дописан — разбор продолжается с конца разобранного.
type scanCache struct {
	file   string
	size   int64
	mtime  time.Time
	since  time.Time
	offset int64 // конец разобранных целых строк
	scan   *transcriptScan
}

// read — незакрытая работа по файлу транскрипта. Транскрипт только дописывается; файл стал короче разобранного
// или другой — разбор заново.
func (c *scanCache) read(file string, info os.FileInfo, since, now time.Time) (transcriptFacts, error) {
	same := c.scan != nil && c.file == file && c.since.Equal(since)
	if same && c.size == info.Size() && c.mtime.Equal(info.ModTime()) {
		return c.scan.facts(now), nil
	}
	f, err := os.Open(file)
	if err != nil {
		return transcriptFacts{}, fmt.Errorf("транскрипт: %w", err)
	}
	defer f.Close()
	if !same || info.Size() < c.offset {
		*c = scanCache{file: file, since: since, scan: newScan(since)}
	} else if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		c.scan = nil
		return transcriptFacts{}, fmt.Errorf("транскрипт: %w", err)
	}
	n, err := c.scan.feed(f)
	if err != nil {
		c.scan = nil // разбор неполон: в следующий раз — заново
		return transcriptFacts{}, err
	}
	c.offset += n
	c.size, c.mtime = info.Size(), info.ModTime()
	return c.scan.facts(now), nil
}
