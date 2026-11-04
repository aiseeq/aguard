package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/queue"
)

const (
	queueExternalEvery   = 5 * time.Minute  // запрос к трекеру идёт секунды на проект: каждые 30 с незачем, письмо о HIGH подождёт
	queueExternalTimeout = 10 * time.Second // сам tk держит источник 5 с
	queueExternalSweep   = time.Minute      // весь обход: он идёт до писем, местные не должны ждать дольше
	// После таймаута: потомок tk с открытыми трубами иначе держит Output до своего конца.
	queueExternalKillWait = 2 * time.Second
)

// queueExternal — задачи внешнего трекера как тикеты: то же, что мастер видит в tk next. Правила отбора и доверие к источнику
// остаются в tk. Сбой проекта — WARN и прошлый список: местные тикеты идут как шли, отметки о письмах целы.
func (a *app) queueExternal(ctx context.Context, c *config.Config, state *queue.State, now time.Time) ([]queue.Ticket, error) {
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil {
		return nil, err
	}
	projects := queueExternalProjects(&c.Queue)
	if now.Sub(a.queueExternalAt) >= queueExternalEvery {
		a.queueExternalAt = now
		a.pollQueueExternal(ctx, c.Queue.Tk, projects, state, now, queueExternalSweep)
	}
	state.KeepExternal(projects)
	var out []queue.Ticket
	for _, dir := range projects {
		master, admin := queueOwners(dir, a.home, c.Lead, table, c.Admins)
		for _, task := range state.External[dir] {
			out = append(out, task.Ticket(filepath.Base(dir), dir, master, admin))
		}
	}
	return out, nil
}

func (a *app) pollQueueExternal(ctx context.Context, tk string, projects []string, state *queue.State, now time.Time, budget time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, dir := range projects {
		fresh, err := readQueueExternal(ctx, tk, dir)
		if err != nil {
			a.warn("queue", "внешний трекер", "project", dir, "err", err)
			continue
		}
		state.SetExternal(dir, fresh, now)
	}
}

// queueExternalProjects — папки проектов из live в корнях, кроме skip. Папка с иным сбоем остаётся: причину покажет опрос, список цел.
func queueExternalProjects(cfg *queue.Config) []string {
	var out []string
	for _, root := range cfg.GitRoots {
		for _, name := range cfg.Live {
			dir := filepath.Join(root, name)
			if slices.Contains(cfg.Skip, dir) {
				continue
			}
			info, err := os.Stat(dir)
			if errors.Is(err, os.ErrNotExist) { // имя из live есть не в каждом корне
				continue
			}
			if err == nil && !info.IsDir() {
				continue
			}
			out = append(out, dir)
		}
	}
	return out
}

func readQueueExternal(ctx context.Context, tk, dir string) ([]queue.ExternalTask, error) {
	ctx, cancel := context.WithTimeout(ctx, queueExternalTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tk, "external", "--json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(tk)+string(os.PathListSeparator)+os.Getenv("PATH")) // источник зовёт tk по имени, а у юнита короткий PATH
	cmd.WaitDelay = queueExternalKillWait
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var tasks []queue.ExternalTask
	if err := json.Unmarshal(stdout, &tasks); err != nil {
		return nil, fmt.Errorf("разбор: %w", err)
	}
	return tasks, nil
}
