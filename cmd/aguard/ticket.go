package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/names"
)

// classTicket — тикет класса в папке хозяина (наблюдатели io и disk, «одна логика на действие»): заметка в открытый
// <base>[-N] или новый со ссылкой на последний закрытый. Хозяина нет — лиду; папки хозяина нет — лиду с его именем.
type classTicket struct {
	watcher string // io, disk: журнал и учёт
	owner   string // "" — тикет лиду
	lead    string
	tk      string
	log     bool   // mode = log: только журнал «завёл бы»
	base    string // slug класса: io-freeze-go, disk-stale
	title   string
	what    string // класс для учёта «завёл бы»
	count   int64  // находок или случаев в тикете
	unit    string // cases, records
	// text — текст нового тикета: prev — последний закрытый тикет класса, redirect — «lead (за хозяин)», когда папки
	// хозяина нет и тикет лиду.
	text func(prev, redirect string) string
	note string // заметка в открытый тикет
}

// classTicket — тикет или заметка; ошибка — tk не ответил или упал.
func (a *app) classTicket(ctx context.Context, t classTicket) error {
	who := t.owner
	if who == "" {
		who = t.lead
	}
	dir, err := a.sessionDir(who, t.lead)
	redirect := ""
	if err != nil { // хозяин есть, его папки нет (задание бойца от сессии без проекта): тикет лиду с именем хозяина
		a.warn(t.watcher, "папка хозяина не найдена, тикет — лиду", "owner", who, "err", err)
		dir, redirect = a.home, t.lead+" (за "+who+")"
	}
	if t.log {
		a.log.Info("aguard "+t.watcher+": завёл бы тикет", "dir", dir, "slug", t.base, t.unit, t.count)
		a.note(t.watcher, "ticket_would", t.what, who, t.count, t.unit)
		return nil
	}
	open, next, prev, err := findClassTicket(ctx, t.tk, dir, t.base)
	if err != nil {
		return err
	}
	if open != "" {
		if err := tkRun(ctx, t.tk, dir, nil, "note", open, t.note, "--by", "aguard"); err != nil {
			return err
		}
		a.note(t.watcher, "ticket_note", open, who, t.count, t.unit)
		return nil
	}
	args := []string{"new", next, "--prio", "MED", "--diff", "HUNT", "--by", "aguard", "--title", t.title, "--text", "-"}
	if err := tkRun(ctx, t.tk, dir, strings.NewReader(t.text(prev, redirect)), args...); err != nil {
		return err
	}
	a.note(t.watcher, "ticket_new", next, who, t.count, t.unit)
	return nil
}

type tkRow struct {
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

var tkClosed = map[string]bool{"готово": true, "отклонён": true, "перенесён": true}

// findClassTicket — открытый тикет класса (base или base-N); иначе свободный slug и последний закрытый (ссылка в новом).
func findClassTicket(ctx context.Context, tk, dir, base string) (open, next, prev string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tk, "ls", "--all", "--slug", base, "--json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("tk ls в %s: %w", dir, err)
	}
	var rows []tkRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", "", "", fmt.Errorf("tk ls в %s: %w", dir, err)
	}
	maxN := 0
	for _, r := range rows {
		n, ok := classSlugN(r.Slug, base)
		if !ok {
			continue
		}
		if !tkClosed[r.Status] {
			return r.Slug, "", "", nil
		}
		if n >= maxN {
			maxN, prev = n, r.Slug
		}
	}
	if prev == "" {
		return "", base, "", nil
	}
	return "", base + "-" + strconv.Itoa(maxN+1), prev, nil
}

// classSlugN — slug класса base: сам base (номер 1) или base-N; иное (base-gopls) — не класс.
func classSlugN(slug, base string) (int, bool) {
	if slug == base {
		return 1, true
	}
	rest, ok := strings.CutPrefix(slug, base+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 1
}

func tkRun(ctx context.Context, tk, dir string, stdin *strings.Reader, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tk, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tk %s в %s: %w: %s", args[0], dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sessionDir — папка тикетов сессии: таблица имён cl, папка админа, <проект>-master — ~/git или ~/work, лид — домашняя.
func (a *app) sessionDir(who, lead string) (string, error) {
	if who == lead {
		return a.home, nil
	}
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil {
		return "", err
	}
	for _, dir := range slices.Sorted(maps.Keys(table)) {
		if table[dir] == who {
			return dir, nil
		}
	}
	c, err := a.config()
	if err != nil {
		return "", err
	}
	for _, dir := range slices.Sorted(maps.Keys(c.Admins)) {
		if c.Admins[dir] == who {
			return dir, nil
		}
	}
	if p, ok := strings.CutSuffix(who, "-master"); ok && p != "" {
		for _, space := range []string{"git", "work"} {
			dir := filepath.Join(a.home, space, p)
			if st, err := os.Stat(dir); err == nil && st.IsDir() && names.For(dir, a.home, table) == who {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("папка сессии %s не найдена (таблица имён cl, admins, ~/git|~/work/<проект>)", who)
}
