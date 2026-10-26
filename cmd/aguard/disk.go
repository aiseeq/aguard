// Наблюдатель disk: рост папок кэшей и состояний, остатки временного, быстрое наполнение (internal/disk). Режим log:
// журнал ~/.local/state/aguard/disk/<день>.jsonl — кому и что сообщил бы; замеры между проходами — state.json там же.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/disk"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/policy"
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
