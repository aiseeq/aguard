// Наблюдатель disk: рост папок кэшей и состояний, остатки временного, быстрое наполнение (internal/disk). Режим log:
// журнал ~/.local/state/aguard/disk/<день>.jsonl — кому и что сообщил бы; замеры между проходами — state.json там же.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/waits"
)

func (a *app) diskCmd(args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("disk tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "обойти записи сейчас и напечатать замеры, ничего не писать")
		parse(fl, rest)
		if *dry {
			return a.diskDry(&cfg)
		}
		return a.diskRound(&cfg)
	case "retro":
		fl := flag.NewFlagSet("disk retro", flag.ContinueOnError)
		days := fl.Int("days", 7, "за сколько дней")
		parse(fl, rest)
		return a.diskRetro(&cfg, *days)
	case "traces":
		fl := flag.NewFlagSet("disk traces", flag.ContinueOnError)
		all := fl.Bool("all", false, "кроме находок — каждый след")
		parse(fl, rest)
		return a.diskTracesNow(&cfg, *all)
	case "report":
		fl := flag.NewFlagSet("disk report", flag.ContinueOnError)
		since := fl.Duration("since", 7*24*time.Hour, "за какой срок")
		parse(fl, rest)
		var recs []disk.Record
		bad, err := disk.NewStore(a.dir("disk")).Read(time.Now().Add(-*since), func(r *disk.Record) { recs = append(recs, *r) })
		if err != nil {
			return err
		}
		return disk.Print(a.out, recs, bad)
	}
	usage()
	return nil
}

// diskOwners — хозяева записей: таблица конфига, живые сессии, админы, лид.
func (a *app) diskOwners(cfg *config.Config) *disk.Owners {
	o := &disk.Owners{Table: cfg.Disk.Owners, Admins: cfg.Admins, Lead: cfg.Lead, Projects: a.diskProjects()}
	infos, err := a.reg.List()
	if err != nil { // часть реестра не прочиталась: хозяева — по остальным
		a.warn("disk", "реестр сессий", "err", err)
	}
	for i := range infos {
		if in := &infos[i]; in.Name != "" && a.reg.IsAlive(in.Pid, in.ProcStart) {
			o.Live = append(o.Live, disk.Session{Name: in.Name, ID: in.SessionID, Cwd: in.Cwd})
		}
	}
	return o
}

// diskProjects — имена сессий папок проектов по таблице cl: её папки и проекты прямо в ~/git и ~/work.
func (a *app) diskProjects() map[string]string {
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil { // без таблицы хозяева — по проектам ~/git и ~/work
		a.warn("disk", "таблица имён cl", "err", err)
	}
	dirs := slices.Collect(maps.Keys(table))
	for _, space := range []string{"git", "work"} {
		des, err := os.ReadDir(filepath.Join(a.home, space))
		if err != nil {
			a.warn("disk", "папки проектов", "err", err)
			continue
		}
		for _, de := range des {
			if de.IsDir() {
				dirs = append(dirs, filepath.Join(a.home, space, de.Name()))
			}
		}
	}
	out := make(map[string]string, len(dirs))
	for _, d := range dirs {
		if name := names.For(d, a.home, table); name != "" {
			out[disk.ProjectKey(d)] = name
		}
	}
	return out
}

// diskRound — один проход: свободное место, по сроку или тревоге — обход записей и сигналы в журнал.
func (a *app) diskRound(cfg *config.Config) error {
	now := time.Now()
	free, err := disk.Free(a.home)
	if err != nil {
		return err
	}
	statePath := filepath.Join(a.dir("disk"), "state.json")
	st, err := disk.Load(statePath)
	if err != nil {
		return err
	}
	owners := a.diskOwners(cfg)
	recs, usage, err := st.Pass(&cfg.Disk, now, free, cfg.Disk.Entries, cfg.Disk.MeasureAll, owners.Of)
	if err != nil { // список записей неполон: сигналы по прочитанным всё равно пишем
		a.warn("disk", "записи", "err", err)
	}
	for path, u := range usage {
		if u.Unread > 0 {
			a.log.Debug("aguard disk: не прочиталось", "path", path, "unread", u.Unread, "err", u.Err)
		}
	}
	if usage != nil { // был обход: следы по свежим замерам
		recs = append(recs, a.diskTraces(cfg, st, owners, now, free, usage)...)
	}
	for i := range recs {
		a.log.Info("aguard disk: " + recs[i].Line(a.home))
	}
	store := disk.NewStore(a.dir("disk"))
	if err := store.Append(recs); err != nil {
		return err
	}
	if err := st.Save(statePath); err != nil {
		return err
	}
	return store.Rotate(now, cfg.Disk.Keep.D())
}

// diskRetro — какие записи выросли бы на grow за день, по ctime файлов, что лежат сейчас (удалённое не видно).
func (a *app) diskRetro(cfg *config.Config, days int) error {
	entries, err := cfg.Disk.Entries()
	if err != nil {
		a.warn("disk", "записи", "err", err)
	}
	owners := a.diskOwners(cfg)
	y, m, d := time.Now().AddDate(0, 0, -days).Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	var b strings.Builder
	for _, e := range entries {
		byDay, err := disk.ByDay(e, !cfg.Disk.IsLasting(e))
		if err != nil { // нечитаемое в записи: прикидка по прочитанному
			a.warn("disk", "retro: не прочиталось "+e, "err", err)
		}
		for day, n := range byDay {
			if !day.Before(from) && n >= int64(cfg.Disk.Grow) {
				owner, why := owners.Of(e)
				fmt.Fprintf(&b, "%s\t%s\t%s (%s)\t%s\n", day.Format(time.DateOnly), policy.HumanBytes(n), owner, why, e)
			}
		}
	}
	lines := strings.SplitAfter(b.String(), "\n")
	slices.Sort(lines)
	if _, err := io.WriteString(a.out, strings.Join(lines, "")); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	return nil
}

// diskDry — замер всех записей сейчас: размер, файлы, последнее изменение, хозяин.
func (a *app) diskDry(cfg *config.Config) error {
	entries, err := cfg.Disk.Entries()
	if err != nil {
		a.warn("disk", "записи", "err", err)
	}
	owners := a.diskOwners(cfg)
	usage := cfg.Disk.MeasureAll(entries)
	enc := json.NewEncoder(a.out)
	for _, e := range entries {
		u := usage[e]
		owner, why := owners.Of(e)
		row := struct {
			Path    string    `json:"path"`
			Size    string    `json:"size"`
			Bytes   int64     `json:"bytes"`
			Blocks  string    `json:"du"`
			Files   int64     `json:"files"`
			Changed time.Time `json:"changed"`
			Unread  int       `json:"unread,omitempty"`
			Err     string    `json:"err,omitempty"`
			Owner   string    `json:"owner"`
			Why     string    `json:"why"`
		}{e, policy.HumanBytes(u.Bytes), u.Bytes, policy.HumanBytes(u.Blocks), u.Files, u.Changed, u.Unread, errText(u.Err), owner, why}
		if err := enc.Encode(&row); err != nil {
			return fmt.Errorf("вывод: %w", err)
		}
	}
	return nil
}

// errText — текст ошибки или пусто.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// graftTimeout — сухой прогон graft worktree clean по одному репозиторию.
const graftTimeout = 2 * time.Minute

// diskFacts — что известно о записях помимо замера: живые сессии, имена сессий по транскриптам (waits), создатели
// по снимку tmpguard-track-home, вердикты graft о worktree, время создания по statx.
func (a *app) diskFacts(cfg *config.Config, owners *disk.Owners, list []string) *disk.Facts {
	f := &disk.Facts{Live: map[string]bool{}, Names: map[string]string{}, Created: map[string]disk.Creator{},
		Verdicts: map[string]disk.Verdict{}}
	for _, s := range owners.Live {
		f.Live[s.ID] = true
	}
	if st, err := waits.LoadState(filepath.Join(a.dir("waits"), "offsets.json")); err != nil {
		a.warn("disk", "имена сессий (waits)", "err", err)
	} else {
		for rel, file := range st.Files {
			if id := disk.SessionID(rel); id != "" && file.Name != "" {
				f.Names[id] = file.Name
			}
		}
	}
	a.diskCreators(cfg, f)
	repos, err := disk.WorktreeRepos(list)
	if err != nil { // часть worktree не прочиталась: вердикты по остальным
		a.warn("disk", "репозитории worktree", "err", err)
	}
	for _, repo := range slices.Sorted(maps.Keys(repos)) {
		ctx, cancel := context.WithTimeout(context.Background(), graftTimeout)
		cmd := exec.CommandContext(ctx, "graft", "worktree", "clean")
		cmd.Dir = repo
		out, err := cmd.Output()
		cancel()
		disk.ParseVerdicts(out, f.Verdicts)
		if err == nil {
			continue
		}
		// graft не судит репозиторий (нет remote): его worktree — следы, только если давно не менялись
		why := err.Error()
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && len(ee.Stderr) > 0 {
			why = lastLine(ee.Stderr)
		}
		a.log.Info("aguard disk: graft worktree clean", "repo", repo, "err", why)
		for _, p := range repos[repo] {
			if _, ok := f.Verdicts[p]; !ok {
				f.Verdicts[p] = disk.Verdict{Word: disk.NoVerdict, Reason: why}
			}
		}
	}
	f.Born = func(path string) time.Time {
		t, err := disk.Btime(path)
		if err != nil { // запись исчезла после обхода: время создания неизвестно
			a.log.Debug("aguard disk: время создания", "err", err)
			return time.Time{}
		}
		return t
	}
	return f
}

// diskCreators — создатели записей по самому свежему снимку tmpguard-track-home; снимка нет — создатели неизвестны.
func (a *app) diskCreators(cfg *config.Config, f *disk.Facts) {
	var best *record.State
	for _, path := range cfg.Disk.Track {
		s, err := record.ReadState(path)
		if err != nil {
			a.warn("disk", "снимок tmpguard-track "+path, "err", err)
			continue
		}
		if best == nil || s.Updated.After(best.Updated) {
			best = s
		}
	}
	if best == nil {
		return
	}
	for i := range best.Entries {
		e := &best.Entries[i]
		cr := disk.Creator{Born: e.Born}
		if o := e.Owner; o != nil {
			cr.Command = o.Proc.Args
			if o.Session != nil {
				cr.Session = o.Session.Name
			}
		}
		f.Created[e.Path] = cr
	}
}

// diskTraces — следы среди замеренных записей и записи журнала о них.
func (a *app) diskTraces(cfg *config.Config, st *disk.State, owners *disk.Owners, now time.Time, free int64,
	usage map[string]disk.Usage) []disk.Record {
	list := slices.Sorted(maps.Keys(usage))
	traces := cfg.Disk.FindTraces(now, list, usage, a.diskFacts(cfg, owners, list), owners.Of)
	exists := func(path string) bool {
		_, err := os.Lstat(path)
		return !errors.Is(err, fs.ErrNotExist)
	}
	return st.Account(&cfg.Disk, now, traces, free, a.home, exists, owners.AdminOf)
}

// diskTracesNow — следы на диске сейчас: обход всех записей, находки по классам, с -all — каждый след; ничего не пишет.
func (a *app) diskTracesNow(cfg *config.Config, all bool) error {
	now := time.Now()
	entries, err := cfg.Disk.Entries()
	if err != nil {
		a.warn("disk", "записи", "err", err)
	}
	free, err := disk.Free(a.home)
	if err != nil {
		return err
	}
	owners := a.diskOwners(cfg)
	usage := cfg.Disk.MeasureAll(entries)
	traces := cfg.Disk.FindTraces(now, entries, usage, a.diskFacts(cfg, owners, entries), owners.Of)
	var b strings.Builder
	findings := cfg.Disk.Findings(now, traces)
	for i := range findings {
		g := &findings[i]
		fmt.Fprintf(&b, "%s → %s (%s)", g.Text(a.home), g.Owner, g.Why)
		if admin := owners.AdminOf(g.Owner); admin != "" {
			fmt.Fprintf(&b, " через %s", admin)
		}
		b.WriteByte('\n')
	}
	if all {
		sort.Slice(traces, func(i, j int) bool { return traces[i].Bytes > traces[j].Bytes })
		for i := range traces {
			t := &traces[i]
			r := disk.Record{Kind: disk.KindTrace, At: now, Free: free, Path: t.Path, Bytes: t.Bytes, Since: t.Born, Owner: t.Owner,
				Why: t.Why, Admin: owners.AdminOf(t.Owner), Trace: t}
			b.WriteString(r.Line(a.home))
			b.WriteByte('\n')
		}
	}
	if _, err := io.WriteString(a.out, b.String()); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	return nil
}

// lastLine — последняя непустая строка вывода.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
