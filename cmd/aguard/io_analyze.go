package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/akit/proc"
)

// Разбор фризов агентом: текст `aguard io at` по главному событию и список событий — команде из
// конфига, её ответ — письмом лиду и сессии-виновнику.

const ioAnalyzePrompt = "Ниже телеметрия фриза ноутбука: по секундам простой PSI, своп, диск и итог по процессам и cgroup. " +
	"Определи, кто вызвал фриз: процесс, cgroup и сессию агента, если это scope сессии; чем — записью, чтением или подкачкой свопа; " +
	"разовое это или повтор (см. список событий); что сделать и кому — владельцу сессии или пользователю. " +
	"Ответь одним абзацем до 8 строк, без вступления.\n"

const (
	ioPromptLimit  = 64 * 1024        // аргумент команды: предел ядра на один аргумент — 128 КБ
	ioPromptAround = 30 * time.Second // секунды вокруг главного события
	ioLongEvent    = 2 * time.Minute  // длиннее — в промпт только секунды с простоем
	ioListedEvents = 50               // событий в списке промпта, остальные — числом
)

// ioAnalysis — вход разбора и адресаты.
type ioAnalysis struct {
	prompt string
	owner  string // сессия главного виновника; "" — не сессия
	cause  string // io или память — чей PSI full больше в главном событии
}

// analyzeIO — разбор событий и письмо. Без блокировки замеров: зовётся из своей горутины.
func (a *app) analyzeIO(ctx context.Context, cfg iowatch.Config, dir string, events []iowatch.Event) error {
	if len(events) == 0 {
		return nil
	}
	main := events[0]
	for _, ev := range events[1:] {
		if ev.IdleUS > main.IdleUS {
			main = ev
		}
	}
	in, err := a.ioAnalysisInput(dir, cfg, events, main)
	if err != nil {
		return err
	}
	argv := ioPromptArgs(cfg.Analyze, in.prompt)
	command, err := findIOCommand(argv[0], a.home)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.AnalyzeTimeout.D())
	defer cancel()
	out, err := runIOAnalyzer(runCtx, command, argv[1:]...)
	if runCtx.Err() != nil {
		return fmt.Errorf("команда разбора %s не уложилась в %s", command, cfg.AnalyzeTimeout.D())
	}
	if err != nil {
		return fmt.Errorf("команда разбора %s: %w: %.500s", command, err, bytes.TrimSpace(out))
	}
	what := in.cause
	if more := len(events) - 1; more > 0 {
		what += fmt.Sprintf(", ещё событий: %d", more)
	}
	message := fmt.Sprintf("aguard io: фриз %s, %d мс (%s)\n%s\nСнимок: %s", main.Start.Format("15:04:05"), main.IdleUS/1000, what,
		strings.TrimSpace(string(out)), main.Path)
	recipients := ioRecipients(cfg.Lead, in.owner)
	if cfg.Mode == iowatch.ModeLog {
		a.log.Info("aguard io: написал бы", "to", strings.Join(recipients, ","), "message", message)
		a.note("io", "написал бы", main.Start.Format(time.RFC3339), strings.Join(recipients, ","), main.IdleUS/1000, "ms")
		return nil
	}
	alive, err := a.reg.Alive()
	if err != nil {
		return fmt.Errorf("реестр сессий: %w", err)
	}
	var sendErr error
	for _, to := range recipients {
		if _, ok := alive[to]; !ok && to != cfg.Lead {
			a.log.Info("aguard io: сессия-виновник уже закрыта, письмо только лиду", "session", to)
			continue
		}
		if err := a.sendAlive(alive, to, cfg.SendAuth, message); err != nil {
			sendErr = errors.Join(sendErr, fmt.Errorf("письмо %s: %w", to, err))
			continue
		}
		a.note("io", "analysis_sent", main.Start.Format(time.RFC3339), to, main.IdleUS/1000, "ms")
	}
	return sendErr
}

// ioRecipients — лид всегда, сессия-виновник — если она есть и это не лид.
func ioRecipients(lead, owner string) []string {
	recipients := []string{lead}
	if owner != "" && owner != lead {
		recipients = append(recipients, owner)
	}
	return recipients
}

// ioAnalysisInput — промпт не больше ioPromptLimit: инструкция, список событий строкой на каждое
// (начало, длительность, простой, главный виновник), итог виновников главного события ±30 с, затем
// его секунды (у длинного — только с простоем). Не влезает — режутся только секунды.
func (a *app) ioAnalysisInput(dir string, cfg iowatch.Config, events []iowatch.Event, main iowatch.Event) (ioAnalysis, error) {
	var in ioAnalysis
	var b strings.Builder
	b.WriteString(ioAnalyzePrompt)
	fmt.Fprintf(&b, "События (%d):\n", len(events))
	for i, ev := range events {
		if i == ioListedEvents {
			fmt.Fprintf(&b, "… и ещё %d\n", len(events)-i)
			break
		}
		win, err := iowatch.ReadSnapshots(dir, ev.Start.Add(-time.Second), ev.End.Add(time.Second), cfg.Ring.D(), a.ioSnapshotWarn)
		if err != nil {
			return in, err
		}
		culprit := "нет данных"
		c := iowatch.SumCulprits(win.Details)
		if path := c.MainCgroup(); path != "" {
			culprit = iowatch.CgroupName(path)
		}
		fmt.Fprintf(&b, "%s, длительность %d мс, простой %d мс, пик шага %d мс, главный виновник: cgroup %s\n",
			ev.Start.Format("2006-01-02 15:04:05"), (ev.End.Sub(ev.Start) + cfg.Step.D()).Milliseconds(), ev.IdleUS/1000, ev.PeakUS/1000, culprit)
	}
	win, err := iowatch.ReadSnapshots(dir, main.Start.Add(-ioPromptAround), main.End.Add(ioPromptAround), cfg.Ring.D(), a.ioSnapshotWarn)
	if err != nil {
		return in, err
	}
	in.cause = ioCause(win, main)
	eventWin, err := iowatch.ReadSnapshots(dir, main.Start.Add(-time.Second), main.End.Add(time.Second), cfg.Ring.D(), a.ioSnapshotWarn)
	if err != nil {
		return in, err
	}
	c := iowatch.SumCulprits(eventWin.Details)
	if name, _, ok := proc.SessionScope(c.MainCgroup()); ok {
		in.owner = name
	}
	fmt.Fprintf(&b, "Главное событие %s, виновники за ±%s:\n", main.Start.Format("15:04:05"), ioPromptAround)
	writeIOSummary(&b, win)
	b.WriteString("По секундам:\n")
	var seconds bytes.Buffer
	writeIOSeconds(&seconds, win, main.End.Sub(main.Start) > ioLongEvent)
	text := seconds.Bytes()
	if room := ioPromptLimit - b.Len(); len(text) > room {
		text = text[:max(0, room)]
		if cut := bytes.LastIndexByte(text, '\n'); cut >= 0 {
			text = text[:cut+1]
		}
	}
	b.Write(text)
	in.prompt = b.String()
	return in, nil
}

// ioCause — чей простой больше за событие: io или память.
func ioCause(win iowatch.Window, ev iowatch.Event) string {
	var ioFull, memFull int64
	for i := range win.Steps {
		if s := &win.Steps[i]; !s.At.Before(ev.Start) && !s.At.After(ev.End) {
			ioFull += s.IOFullUS
			memFull += s.MemFullUS
		}
	}
	if memFull > ioFull {
		return "память"
	}
	return "io"
}

// ioPromptArgs — argv с {prompt}, заменённым текстом; нет {prompt} — текст последним аргументом.
func ioPromptArgs(analyze []string, prompt string) []string {
	argv := make([]string, len(analyze), len(analyze)+1)
	found := false
	for i, arg := range analyze {
		if strings.Contains(arg, "{prompt}") {
			found = true
			arg = strings.ReplaceAll(arg, "{prompt}", prompt)
		}
		argv[i] = arg
	}
	if !found {
		argv = append(argv, prompt)
	}
	return argv
}

// findIOCommand — путь команды: у службы systemd PATH может не содержать ~/.local/bin и ~/bin.
func findIOCommand(name, home string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("команда разбора не найдена: %s", name)
	}
	for _, dir := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("команда разбора %s не найдена в PATH, ~/.local/bin и ~/bin", name)
}

// runIOAnalyzer — команда в своей группе процессов; по таймауту группа убивается целиком, а вывод
// не ждёт внуков, унаследовавших stdout, дольше WaitDelay.
func runIOAnalyzer(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	return cmd.CombinedOutput()
}
