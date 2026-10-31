package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// Сводки случаев фриза: строка и разбор случая, итог за период (`aguard io report`) и перевод
// старых снимков событий в случаи (`aguard io import`).

// ioCaseLine — строка случая: начало, длительность, простой и чей он, главный виновник.
func ioCaseLine(c *iowatch.Case) string {
	cause := "io"
	if c.MemFullMS > c.IOFullMS {
		cause = "память"
	}
	line := fmt.Sprintf("%s, %s (простой %s, %s; пик шага %d мс)", c.Start.Format("02.01 15:04:05"), ioSeconds(c.DurMS),
		ioSeconds(c.IdleMS), cause, c.PeakMS)
	if attribution := ioCaseAttribution(c); attribution != "" {
		line += ", " + attribution
	}
	return line
}

// ioCaseAttribution — одна подпись виновника для заголовка и кратких списков случаев.
func ioCaseAttribution(c *iowatch.Case) string {
	if c.Culprits == nil || c.Culprits.Main == "" {
		return ""
	}
	k := c.Culprits
	main := iowatch.CgroupName(k.Main)
	if k.MainValue > 0 {
		if k.KernelWrite > k.MainValue && k.KernelWrite >= k.Foreign.Value {
			percent := int64(0)
			if k.DeviceWrite > 0 {
				percent = (100*k.KernelWrite + k.DeviceWrite/2) / k.DeviceWrite
			}
			return fmt.Sprintf("фон ядра (writeback, коммит btrfs): %.0f МБ, %d%% записи устройства; главная cgroup %s",
				megabytes(k.KernelWrite), percent, main)
		}
		if k.Foreign.Value > k.MainValue {
			return fmt.Sprintf("чужая cgroup %s (чужой uid): %.0f МБ; главная cgroup %s",
				k.Foreign.Name, megabytes(k.Foreign.Value), main)
		}
	}
	line := "главная cgroup " + main
	if f := k.Foreign; f.Value > 0 {
		line += fmt.Sprintf(" (неточно: у %s чужого uid по io.stat %.0f МБ записи, чтения и свопа — больше, а своя её запись не видна; "+
			"бывает и коммит btrfs по её fsync)", f.Name, megabytes(f.Value))
	}
	return line
}

// writeIOCase — машина за случай и до names виновников на меру.
func writeIOCase(w io.Writer, c *iowatch.Case, names int) {
	fmt.Fprintf(w, "машина: своп in/out %d/%d стр, majflt %d, диск R/W %.0f/%.0f МБ, занят %d мс, ожидание R/W %d/%d мс, очередь до %d, flush %d, dirty %.0f МБ, writeback %.0f МБ\n",
		c.SwapIn, c.SwapOut, c.Majflt, megabytes(c.ReadB), megabytes(c.WriteB), c.BusyMS, c.RAwaitMS, c.WAwaitMS, c.InFlight,
		c.Flushes, megabytes(c.Dirty), megabytes(c.Writeback))
	k := c.Culprits
	if k == nil {
		return
	}
	fmt.Fprintf(w, "виновники с %s:\n", k.From.Format("15:04:05"))
	if k.DeviceWrite > 0 {
		fmt.Fprintf(w, "запись диска за окно %.0f МБ, из них фон ядра %.0f МБ (writeback, коммит и метаданные btrfs вне cgroup — "+
			"порождает их запись процессов ниже)\n", megabytes(k.DeviceWrite), megabytes(k.KernelWrite))
	}
	top := func(list []iowatch.Ranked) []iowatch.Ranked { return list[:min(names, len(list))] }
	writeProcessIOTop(w, "запись процессов (данные и метаданные)", top(k.ProcWrite), ioBytes, k.ProcessDetails)
	writeProcessIOTop(w, "чтение процессов", top(k.ProcRead), ioBytes, k.ProcessDetails)
	writeIOTop(w, "своп процессов", top(k.ProcMajflt), ioMajflt)
	if k.CgroupDirty != nil { // случаи до 30.10 — без неё
		writeIOTop(w, "запись процессов по cgroup", top(k.CgroupDirty), ioBytes)
	}
	writeIOCgroupWrite(w, top(k.CgroupWrite), k.CgroupOwn)
	writeIOTop(w, "чтение cgroup", top(k.CgroupRead), ioBytes)
	writeIOTop(w, "своп cgroup", top(k.CgroupSwapIn), ioPages)
	writeIOTop(w, "простой IO cgroup", top(k.CgroupIdle), ioMillis)
}

// writeProcessIOTop дополняет первые три процесса командой и cwd из детальной записи.
func writeProcessIOTop(w io.Writer, label string, top []iowatch.Ranked, unit int, details map[string]iowatch.ProcessDetails) {
	writeIOTop(w, label, top, unit)
	for _, item := range top[:min(3, len(top))] {
		info := details[item.Name]
		if info.Cmdline == "" {
			continue
		}
		if info.CWD == "" {
			fmt.Fprintf(w, "  cmd: %s\n", info.Cmdline)
		} else {
			fmt.Fprintf(w, "  cmd: %s (в %s)\n", info.Cmdline, info.CWD)
		}
	}
}

// ioFsyncShare и ioFsyncMin — io.stat cgroup больше записи её процессов во столько раз и на столько байт: остальное —
// коммит транзакции btrfs, пришедшийся на её fsync (боец 30.10: 724 МБ при своих ≤3 МБ).
const (
	ioFsyncShare = 4
	ioFsyncMin   = 32 << 20
)

// writeIOCgroupWrite — запись cgroup по io.stat с пометкой коммита по fsync; cgroup не из own — процессы чужого uid;
// own nil — случай до 30.10, без пометок.
func writeIOCgroupWrite(w io.Writer, top []iowatch.Ranked, own map[string]int64) {
	if len(top) == 0 {
		fmt.Fprintf(w, "запись cgroup (io.stat): нет\n")
		return
	}
	parts := make([]string, len(top))
	for i, item := range top {
		note := ""
		mine, ok := own[item.Name]
		switch {
		case own == nil:
		case !ok:
			note = " (процессы чужого uid — своя запись не видна)"
		case mine > 0 && item.Value > ioFsyncShare*mine && item.Value-mine > ioFsyncMin:
			// своих 0 — процессы cgroup не попали в топ шага: доли не знаем
			note = fmt.Sprintf(" (своих %.0f — коммит btrfs по её fsync)", megabytes(mine))
		}
		parts[i] = ioValue(item.Name, item.Value, ioBytes) + note
	}
	fmt.Fprintf(w, "запись cgroup (io.stat): %s\n", strings.Join(parts, ", "))
}

// ioSeconds — длительность в мс человеку: до секунды — мс, дальше — секунды с десятыми.
func ioSeconds(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d мс", ms)
	}
	return ioDecimal(float64(ms)/1000) + " с"
}

// ioDecimal — число с десятыми через запятую: «1,1».
func ioDecimal(x float64) string {
	return strings.Replace(strconv.FormatFloat(x, 'f', 1, 64), ".", ",", 1)
}

// ioReport — итог случаев [since, until): из файла службы (live) — сначала служба пишет сводки из
// памяти (SIGUSR1); не запущена — читается файл как есть.
func (a *app) ioReport(path string, live bool, since, until time.Time, minDur time.Duration, asJSON bool) error {
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	if minDur <= 0 {
		minDur = cfg.CaseMin.D()
	}
	if live {
		if err := ioRequestSave(path); err != nil {
			if !errors.Is(err, errIONotRunning) {
				return err
			}
			a.log.Warn("aguard io report: служба не запущена, сводки — из файла как есть")
		}
	}
	cases, err := iowatch.LoadCases(path)
	if err != nil {
		return err
	}
	sum := iowatch.Summarize(cases, since, until, minDur)
	if asJSON {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(sum)
	}
	writeIOReport(a.out, &sum)
	return nil
}

// ioRequestSave — SIGUSR1 службе и ожидание обновления файла сводок (до 10 с).
func ioRequestSave(path string) error {
	requested := time.Now()
	if err := ioSignal(syscall.SIGUSR1); err != nil {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if info, err := os.Stat(path); err == nil && !info.ModTime().Before(requested) {
			return nil
		}
	}
	return fmt.Errorf("aguard io report: служба не обновила %s за 10 с — см. журнал службы", path)
}

// ioReportCases — сколько самых длинных случаев в тексте итога.
const ioReportCases = 10

// writeIOReport — итог текстом: корзины длительности, часы, главные виновники, самые длинные случаи.
func writeIOReport(w io.Writer, s *iowatch.Summary) {
	fmt.Fprintf(w, "Случаи фриза %s — %s: %d, простой всего %s\n", s.Since.Format("02.01 15:04"), s.Until.Format("02.01 15:04"),
		s.Total, ioSeconds(s.IdleMS))
	if s.Quiet > 0 {
		fmt.Fprintf(w, "тихих (простой без обмена с диском, не фриз): %d, простой %s; кто ждал (случаев от %s): %s\n", s.Quiet,
			ioSeconds(s.QuietIdleMS), ioSeconds(s.Min), ioRankedCases(s.QuietWho, -1))
	}
	if s.Total == 0 {
		return
	}
	parts := make([]string, 0, len(s.Buckets))
	for _, b := range s.Buckets {
		parts = append(parts, fmt.Sprintf("%s %d", b.Label, b.Count))
	}
	fmt.Fprintf(w, "по длительности: %s\n", strings.Join(parts, " · "))
	fmt.Fprintf(w, "по часам (случаев / от %s / самый длинный):\n", ioSeconds(s.Min))
	for _, h := range s.Hours {
		fmt.Fprintf(w, "  %s  %d / %d / %s\n", h.Hour.Format("02.01 15:00"), h.Count, h.Long, ioSeconds(h.MaxDurMS))
	}
	if len(s.Cases) == 0 {
		return
	}
	fmt.Fprintf(w, "случаев от %s: %d; главная cgroup (случаев): %s\n", ioSeconds(s.Min), len(s.Cases), ioRankedCases(s.Main, -1))
	k := &s.Culprits
	for _, row := range []struct {
		label string
		list  []iowatch.RankedCases
		unit  int
	}{
		{"запись процессов", k.ProcWrite, ioBytes}, {"чтение процессов", k.ProcRead, ioBytes}, {"своп процессов (majflt)", k.ProcMajflt, ioMajflt},
		{"запись cgroup", k.CgroupWrite, ioBytes}, {"чтение cgroup", k.CgroupRead, ioBytes}, {"своп cgroup", k.CgroupSwapIn, ioPages},
		{"простой IO cgroup", k.CgroupIdle, ioMillis},
	} {
		fmt.Fprintf(w, "%s: %s\n", row.label, ioRankedCases(row.list, row.unit))
	}
	fmt.Fprintf(w, "самые длинные:\n")
	for i := range s.Cases[:min(ioReportCases, len(s.Cases))] {
		fmt.Fprintf(w, "- %s\n", ioCaseLine(&s.Cases[i]))
	}
}

// ioRankedCases — «имя N ед в K случаях, …»; unit < 0 — только число случаев.
func ioRankedCases(list []iowatch.RankedCases, unit int) string {
	if len(list) == 0 {
		return "нет"
	}
	parts := make([]string, len(list))
	for i, r := range list {
		if unit < 0 {
			parts[i] = fmt.Sprintf("%s %d", r.Name, r.Cases)
			continue
		}
		parts[i] = fmt.Sprintf("%s в %d", ioValue(r.Name, r.Value, unit), r.Cases)
	}
	return strings.Join(parts, ", ")
}

// ioImport — снимки событий прежнего наблюдателя (до сводок случаев) через тот же Recorder, случаи —
// к сводкам в path. В файл службы (live) — только пока она стоит: при остановке она перепишет его
// своей памятью.
func (a *app) ioImport(dir, path string, live, remove bool) error {
	if live {
		if err := ioSignal(0); !errors.Is(err, errIONotRunning) {
			if err == nil {
				return errors.New("aguard io import: служба aguard запущена — остановите её (systemctl --user stop aguard), иначе она перепишет сводки")
			}
			return err
		}
	}
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("папка снимков: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") && !iowatch.IsManual(e.Name()) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(files) // имя — начало события: по времени
	rec := iowatch.NewRecorder(cfg)
	var found []iowatch.Case
	for _, file := range files {
		if err := ioReplay(file, rec, &found, a.ioSnapshotWarn); err != nil {
			return err
		}
	}
	found = append(found, rec.Close()...)
	old, err := iowatch.LoadCases(path)
	if err != nil {
		return err
	}
	if err := iowatch.SaveCases(path, iowatch.MergeCases(old, found, time.Now().Add(-cfg.CasesKeep.D()))); err != nil {
		return err
	}
	long := 0
	for i := range found {
		if found[i].Dur() >= cfg.CaseMin.D() {
			long++
		}
	}
	fmt.Fprintf(a.out, "снимков %d, случаев %d (от %s — %d) → %s\n", len(files), len(found), ioSeconds(cfg.CaseMin.D().Milliseconds()), long, path)
	if !remove {
		return nil
	}
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("удалить снимок: %w", err)
		}
	}
	return nil
}

// ioReplay — записи снимка в Recorder по порядку; готовые случаи — в found.
func ioReplay(file string, rec *iowatch.Recorder, found *[]iowatch.Case, warn func(error)) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("снимок: %w", err)
	}
	defer f.Close()
	return iowatch.ParseSnapshot(f, func(r iowatch.Record) {
		if r.Kind == iowatch.KindDetail {
			rec.Detail(r.Step)
			return
		}
		*found = append(*found, rec.Step(r.Step)...)
	}, warn)
}
