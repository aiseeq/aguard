package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/guiwatch"
)

type pidFlags []guiwatch.Target

func (p *pidFlags) String() string { return "" }
func (p *pidFlags) Set(v string) error {
	name, pidText, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("ожидается имя=PID")
	}
	pid, e := strconv.Atoi(pidText)
	if e != nil || pid <= 0 {
		return fmt.Errorf("неверный PID %q", pidText)
	}
	*p = append(*p, guiwatch.Target{Name: name, PID: pid, Main: true})
	return nil
}

func (a *app) guiCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	switch cmd {
	case "watch":
		fl := flag.NewFlagSet("gui watch", flag.ContinueOnError)
		dur := fl.Duration("for", 10*time.Second, "длительность наблюдения")
		dir := fl.String("dir", filepath.Join(a.home, ".local", "state", "aguard", "gui"), "папка срезов")
		var pids pidFlags
		fl.Var(&pids, "pid", "цель имя=PID (можно повторять)")
		parse(fl, rest)
		if *dur <= 0 {
			return errors.New("gui watch: длительность должна быть положительной")
		}
		cfg, err := a.config()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, *dur)
		defer cancel()
		w := guiwatch.New(procRoot, *dir, cfg.GUI, []guiwatch.Target(pids))
		w.OnTargets = func(line string, missing []string) {
			fmt.Fprintln(os.Stderr, line)
			a.log.Info("aguard gui: " + line)
			if len(missing) > 0 {
				fmt.Fprintf(os.Stderr, "предупреждение: отсутствуют цели %s\n", strings.Join(missing, ", "))
				a.log.Warn("aguard gui: отсутствуют цели", "targets", strings.Join(missing, ", "))
			}
		}
		return w.Watch(ctx)
	case "report":
		fl := flag.NewFlagSet("gui report", flag.ContinueOnError)
		since := fl.Duration("since", 24*time.Hour, "период")
		asJSON := fl.Bool("json", false, "JSON")
		dir := fl.String("dir", filepath.Join(a.home, ".local", "state", "aguard", "gui"), "папка срезов")
		parse(fl, rest)
		if *since < 0 {
			return errors.New("gui report: since не может быть отрицательным")
		}
		d, err := guiwatch.ReadReport(*dir, time.Now().Add(-*since))
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(a.out)
			enc.SetIndent("", "  ")
			return enc.Encode(d)
		}
		fmt.Fprintf(a.out, "Срезов интерфейса: %d, подавлено: %d, самое длинное S по wchan: %s\n", len(d.Slices), d.Suppressed, guiwatch.LongestSleep(d))
		for _, item := range d.Sleeps {
			fmt.Fprintf(a.out, "  %s wchan=%s: %dms (≥0.5/1/2/5s: %d/%d/%d/%d)\n", item.Target, item.Wchan, item.LongestMS, item.Over500MS, item.Over1SMS, item.Over2SMS, item.Over5SMS)
		}
		for i := range d.Slices {
			s := &d.Slices[i]
			fmt.Fprintf(a.out, "%s pid=%d tid=%d %s %s\n", s.Start.Format(time.RFC3339), s.PID, s.TID, s.Kind, time.Duration(s.DurationMS)*time.Millisecond)
		}
		fmt.Fprintln(a.out, "Стеков ядра нет: /proc/*/stack требует root.")
		return nil
	}
	return fmt.Errorf("aguard gui: неизвестная команда %s (watch, report)", cmd)
}

func (a *app) guiLoop(ctx context.Context) {
	dir := a.dir("gui")
	if err := os.MkdirAll(dir, guiwatch.DirectoryMode); err != nil {
		a.log.Error("aguard gui: каталог", "err", err)
		return
	}
	for ctx.Err() == nil {
		cfg, err := a.config()
		if err != nil {
			a.log.Error("aguard gui: конфиг", "err", err)
			waitContext(ctx, time.Second)
			continue
		}
		if cfg.GUI.Mode == guiwatch.ModeOff {
			waitContext(ctx, 5*time.Second)
			continue
		}
		w := guiwatch.New(procRoot, dir, cfg.GUI, nil)
		w.ReloadConfig = func() (guiwatch.Config, error) {
			current, e := a.config()
			if e != nil {
				return guiwatch.Config{}, e
			}
			return current.GUI, nil
		}
		w.OnTargets = func(line string, missing []string) {
			fmt.Fprintln(os.Stderr, line)
			a.log.Info("aguard gui: " + line)
			if len(missing) > 0 {
				a.log.Warn("aguard gui: отсутствуют цели", "targets", strings.Join(missing, ", "))
			}
		}
		if err := w.Watch(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, guiwatch.ErrConfigChanged) {
			a.log.Error("aguard gui: наблюдение", "err", err)
			waitContext(ctx, time.Second)
		}
	}
}

func waitContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
