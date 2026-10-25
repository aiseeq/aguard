package orphans

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aiseeq/aguard/internal/sessions"
)

// Outcome — чем кончилось снятие дерева.
type Outcome struct {
	Tree    *Tree
	Termed  int // получили SIGTERM
	Exited  int // вышли сами после SIGTERM
	Killed  int // добиты SIGKILL
	Gone    int // уже не было (или pid занят другим процессом) к моменту снятия
	Podman  string
	Elapsed time.Duration
	Errs    []error
}

// Text — итог одной строкой для журнала и сводки.
func (o *Outcome) Text() string {
	var parts []string
	if o.Podman != "" {
		parts = append(parts, o.Podman)
	}
	if o.Termed > 0 {
		parts = append(parts, fmt.Sprintf("SIGTERM %d: вышли %d, добито SIGKILL %d", o.Termed, o.Exited, o.Killed))
	}
	if o.Gone > 0 {
		parts = append(parts, fmt.Sprintf("уже не было %d", o.Gone))
	}
	for _, e := range o.Errs {
		parts = append(parts, "ошибка: "+e.Error())
	}
	return strings.Join(parts, "; ") + fmt.Sprintf(" (%s)", o.Elapsed.Round(time.Second))
}

type victim struct {
	fd  int
	p   *Proc
	out *Outcome
}

// killGrace — сколько ждать выхода после SIGKILL.
const killGrace = 5 * time.Second

// Stop снимает деревья: контейнеры — podman stop, остальное — SIGTERM всем
// процессам дерева, через wait — SIGKILL оставшимся. Каждый процесс сверяется
// по времени старта через pidfd: переиспользованный pid не задевается.
func Stop(ctx context.Context, trees []*Tree, wait time.Duration, procRoot string) []Outcome {
	start := time.Now()
	outs := make([]Outcome, len(trees))
	var victims []victim
	for i, t := range trees {
		outs[i].Tree = t
		if t.Container != "" {
			continue
		}
		for _, m := range t.Members {
			fd, err := term(m, procRoot)
			switch {
			case errors.Is(err, errGone):
				outs[i].Gone++
			case err != nil:
				outs[i].Errs = append(outs[i].Errs, err)
			default:
				outs[i].Termed++
				victims = append(victims, victim{fd: fd, p: m, out: &outs[i]})
			}
		}
	}
	for i, t := range trees { // podman stop ждёт сам; процессы тем временем выходят
		if t.Container != "" {
			outs[i].Podman = podmanStop(ctx, t.Container, t.Storage, wait)
		}
	}
	left := waitExit(victims, start.Add(wait))
	for _, v := range victims {
		if !left[v.fd] {
			v.out.Exited++
		}
	}
	var killed []victim
	for _, v := range victims {
		if !left[v.fd] {
			continue
		}
		if err := unix.PidfdSendSignal(v.fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			v.out.Errs = append(v.out.Errs, fmt.Errorf("SIGKILL pid %d: %w", v.p.Pid, err))
			continue
		}
		v.out.Killed++
		killed = append(killed, v)
	}
	stuck := waitExit(killed, time.Now().Add(killGrace))
	for _, v := range killed {
		if stuck[v.fd] {
			v.out.Errs = append(v.out.Errs, fmt.Errorf("pid %d жив через %s после SIGKILL", v.p.Pid, killGrace))
		}
	}
	for _, v := range victims {
		if err := unix.Close(v.fd); err != nil {
			v.out.Errs = append(v.out.Errs, fmt.Errorf("pidfd pid %d: %w", v.p.Pid, err))
		}
	}
	for i := range outs {
		outs[i].Elapsed = time.Since(start)
	}
	return outs
}

// errGone — процесса уже нет или pid занят другим процессом.
var errGone = errors.New("процесса уже нет")

// term открывает pidfd, сверяет старт и шлёт SIGTERM; возвращает открытый pidfd.
func term(m *Proc, procRoot string) (int, error) {
	fd, err := unix.PidfdOpen(m.Pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return -1, errGone
	}
	if err != nil {
		return -1, fmt.Errorf("pidfd pid %d: %w", m.Pid, err)
	}
	// pidfd держит именно этот процесс; сверка после открытия исключает гонку с переиспользованием
	st, err := sessions.ProcStat(procRoot, m.Pid)
	switch {
	case err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH)):
		err = errGone
	case err != nil:
		err = fmt.Errorf("stat pid %d: %w", m.Pid, err)
	case st.Start != m.StartRaw:
		err = errGone
	default:
		err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
		if errors.Is(err, unix.ESRCH) {
			err = errGone
		} else if err != nil {
			err = fmt.Errorf("SIGTERM pid %d: %w", m.Pid, err)
		}
	}
	if err == nil {
		return fd, nil
	}
	if cerr := unix.Close(fd); cerr != nil {
		err = errors.Join(err, fmt.Errorf("pidfd pid %d: %w", m.Pid, cerr))
	}
	return -1, err
}

// waitExit ждёт до deadline, пока процессы выйдут (pidfd становится читаемым).
// Возвращает fd -> ещё жив.
func waitExit(vs []victim, deadline time.Time) map[int]bool {
	left := map[int]bool{}
	for _, v := range vs {
		left[v.fd] = true
	}
	for len(left) > 0 {
		ms := time.Until(deadline).Milliseconds()
		if ms <= 0 {
			break
		}
		pfds := make([]unix.PollFd, 0, len(left))
		for fd := range left {
			pfds = append(pfds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		}
		n, err := unix.Poll(pfds, int(ms))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n == 0 {
			break // ошибка poll или время вышло: оставшиеся считаем живыми
		}
		for _, pf := range pfds {
			if pf.Revents != 0 {
				delete(left, int(pf.Fd))
			}
		}
	}
	return left
}

// podmanStop останавливает контейнер штатно: podman сам шлёт SIGTERM и через -t — SIGKILL.
func podmanStop(ctx context.Context, id string, storage []string, wait time.Duration) string {
	const slack = time.Minute
	ctx, cancel := context.WithTimeout(ctx, wait+slack)
	defer cancel()
	secs := strconv.Itoa(int(wait.Seconds()))
	args := append(append([]string{}, storage...), "stop", "-t", secs, id)
	out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
	res := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Sprintf("podman stop %s: ошибка %v: %s", ShortID(id), err, res)
	}
	return fmt.Sprintf("podman stop -t %s %s: ok", secs, ShortID(id))
}
