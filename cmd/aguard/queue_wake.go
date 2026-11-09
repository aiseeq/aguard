package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/queue"
)

type queueNextResult struct {
	at     time.Time
	dir    string
	ticket queue.Ticket
	found  bool
	err    error
}

// nextForWake спрашивает tk за мастера. Ответ, полученный до его хода, устарел:
// взятый за эти минуты тикет иначе выглядел бы как «первым остался тот же».
func (a *app) nextForWake(ctx context.Context, tk, master, dir string, now time.Time, turned func(time.Time) bool) (queue.Ticket, bool, error) {
	if cached, ok := a.queueNext[master]; ok && turned(cached.at) {
		delete(a.queueNext, master)
	}
	return a.nextForQueue(ctx, tk, master, dir, master, now)
}

func (a *app) nextForAdmin(ctx context.Context, tk, admin, dir string, now time.Time) (queue.Ticket, bool, error) {
	return a.nextForQueue(ctx, tk, admin, dir, "admin\x00"+admin+"\x00"+dir, now)
}
func (a *app) nextForQueue(ctx context.Context, tk, master, dir, cacheKey string, now time.Time) (queue.Ticket, bool, error) {
	if a.queueNext == nil {
		a.queueNext = map[string]queueNextResult{}
	}
	cached, ok := a.queueNext[cacheKey]
	if ok && cached.dir == dir && now.Sub(cached.at) < 5*time.Minute {
		return cached.ticket, cached.found, cached.err
	}
	res := queueNextResult{at: now, dir: dir}
	defer func() { a.queueNext[cacheKey] = res }()
	cmdCtx, cancel := context.WithTimeout(ctx, queueExternalTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, tk, "next", "--json", "--by", master)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(tk)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.WaitDelay = queueExternalKillWait
	b, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && cmdCtx.Err() == nil {
			a.log.Debug("tk next: работы нет", "project", dir)
			return queue.Ticket{}, false, nil
		}
		res.err = fmt.Errorf("tk next %s: %w", dir, err)
		a.warn("queue", "tk next", "err", res.err)
		return queue.Ticket{}, false, res.err
	}
	var raw struct {
		Source   string `json:"source"`
		Key      string `json:"key"`
		Path     string `json:"path"`
		Slug     string `json:"slug"`
		Status   string `json:"status"`
		Executor string `json:"executor"`
		Prio     string `json:"prio"`
	}
	if err = json.Unmarshal(b, &raw); err != nil {
		res.err = fmt.Errorf("tk next %s: %w", dir, err)
		a.warn("queue", "tk next", "err", res.err)
		return queue.Ticket{}, false, res.err
	}
	project := filepath.Base(dir)
	key := queueKey(project, raw.Slug, raw.Path)
	if raw.Source == "external" && raw.Key != "" {
		key = project + "/" + raw.Key
	}
	if key == "" {
		res.err = fmt.Errorf("tk next %s: нет ключа", dir)
		a.warn("queue", "tk next", "err", res.err)
		return queue.Ticket{}, false, res.err
	}
	res.ticket = queue.WakeTicket(project, dir, key)
	res.ticket.Status, res.ticket.Executor, res.ticket.Prio, res.ticket.Slug = raw.Status, raw.Executor, raw.Prio, raw.Slug
	res.found = true
	return res.ticket, true, nil
}

func (a *app) workerOwners() (map[string]bool, error) {
	list := a.queueWorkers
	if list == nil {
		list = func() ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "systemctl", "--user", "list-units", "--type=service", "--state=active", "--plain", "--no-legend", "--no-pager", "worker-*.service").Output()
			if err != nil {
				return nil, err
			}
			var units []string
			for line := range strings.SplitSeq(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					units = append(units, fields[0])
				}
			}
			return units, nil
		}
	}
	units, err := list()
	if err != nil {
		return nil, err
	}
	owners := map[string]bool{}
	for _, unit := range units {
		id, ok := strings.CutPrefix(strings.TrimSuffix(unit, ".service"), "worker-")
		if !ok || id == "" || strings.ContainsAny(id, "/\\") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.home, ".local/state/worker", id, "job.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var job struct {
			Owner struct {
				Name string `json:"name"`
			} `json:"owner"`
		}
		if err = json.Unmarshal(data, &job); err != nil {
			return nil, err
		}
		if job.Owner.Name != "" {
			owners[job.Owner.Name] = true
		}
	}
	return owners, nil
}
