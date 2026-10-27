package main

// Подхват нового cl без перезапуска консоли (задание лида 27.10 14:05 CET: «Пусть cl подхватывает себя сам»): живой cl
// держит старый бинарник, пока консоль открыта, и после установки работал бы по старому коду до ручного перезапуска.
// В точке между двумя запусками claude (перезапуск на новую версию, сжатие, сброс) claude не запущен, терминал уже
// вышел из сырого режима, pty закрыт — cl делает exec в новый бинарник по тому же пути: pid, терминал и вкладка те же,
// следующий запуск claude — с теми же аргументами. Живую беседу это не трогает.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// upgradeEnv — переменная окружения, в которой прежний cl передаёт новому, что поднимать.
const upgradeEnv = "CL_UPGRADE"

// upgradeV — версия формата upgrade: новый cl понимает состояние от старого с той же версией.
const upgradeV = 1

// upgrade — с чем новый cl продолжает: имя сессии, номер запуска (часть имени scope) и аргументы следующего запуска
// claude (без --append-system-prompt: его cl добавляет при каждом запуске).
type upgrade struct {
	V    int      `json:"v"`
	Name string   `json:"name"`
	N    int      `json:"n"`
	Args []string `json:"args"`
	// Pid — claude запуска N жив (liveUpgrade): не запускать, а ждать его; Master — дескриптор master его pty (0 —
	// без pty), Typed — последний ввод человека, unix nano.
	Pid    int   `json:"pid,omitempty"`
	Master int   `json:"master,omitempty"`
	Typed  int64 `json:"typed,omitempty"`
}

// takeUpgrade — состояние от прежнего cl из окружения; переменную убирает, чтобы её не унаследовал claude. ok —
// переменная была; не разобралась — ошибка: cl стартует как обычно.
func takeUpgrade() (u *upgrade, ok bool, err error) {
	s, ok := os.LookupEnv(upgradeEnv)
	if !ok {
		return nil, false, nil
	}
	if err := os.Unsetenv(upgradeEnv); err != nil {
		return nil, true, fmt.Errorf("состояние прежнего cl: %w", err)
	}
	u = &upgrade{}
	if err := json.Unmarshal([]byte(s), u); err != nil {
		return nil, true, fmt.Errorf("состояние прежнего cl: %w", err)
	}
	if u.V != upgradeV || u.Name == "" || len(u.Args) == 0 {
		return nil, true, fmt.Errorf("состояние прежнего cl: версия %d (нужна %d), имя %q, аргументов %d", u.V, upgradeV, u.Name, len(u.Args))
	}
	return u, true, nil
}

// goneSuffix — пометка ядра в конце ссылки /proc/<pid>/exe, когда файл бинарника убран с диска.
const goneSuffix = " (deleted)"

// replaced — путь бинарника cl, если его заменили установкой новой версии: ссылка exeLink (/proc/self/exe) тогда
// кончается на goneSuffix. exeLink пуст (тест) — не подхватывать; ссылка не читается — ошибка.
func replaced(exeLink string) (path string, ok bool, err error) {
	if exeLink == "" {
		return "", false, nil
	}
	target, err := os.Readlink(exeLink)
	if err != nil {
		return "", false, fmt.Errorf("свой бинарник: %w", err)
	}
	path, ok = strings.CutSuffix(target, goneSuffix)
	return path, ok, nil
}

// reexec — exec в новый cl по пути path с состоянием u; строка в журнал до exec. Вернулся — exec не вышел (нового
// бинарника нет, сбой): дальше работает прежний cl.
func (l launcher) reexec(journalFile, path string, u upgrade) error {
	state, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("состояние cl: %w", err)
	}
	live := ""
	if u.Pid > 0 {
		live = fmt.Sprintf(" на ходу, claude pid=%d остаётся", u.Pid)
	}
	journal(journalFile, time.Now(), fmt.Sprintf("%s pid=%d: cl обновлён — перехожу в новый бинарник %s%s", u.Name, os.Getpid(), path, live))
	if err := l.exec(path, os.Args, append(os.Environ(), upgradeEnv+"="+string(state))); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

// upgrade — перед запуском n: бинарник заменён — exec в новый с аргументами args; не вышло — строка в журнал, дальше
// прежний cl.
func (l launcher) upgrade(n int, args []string) {
	path, ok, err := replaced(l.selfExe)
	if !ok && err == nil {
		return
	}
	cfg, cfgErr := loadConfig(l.configFile, l.home)
	if ok {
		err = l.reexec(cfg.Restart.Journal, path, upgrade{V: upgradeV, Name: l.name, N: n, Args: args})
	}
	if err == nil && cfgErr == nil { // exec вернулся без ошибки — только подмена в тесте
		return
	}
	journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s: %v — дальше прежний cl", l.name, errors.Join(err, cfgErr)))
}

// liveUpgrade — бинарник заменён, а claude запуска n жив: exec в новый cl, не трогая claude (задание лида 27.10
// 18:5x CET: «exec нового cl с передачей fd мастера pty и состояния… Перезапуск ради нового cl — только если этот путь
// не выйдет»). Иначе под pty cl не подхватывал себя вовсе: сжатие и сброс набором не перезапускают claude, и старый cl
// жил до настоящего перезапуска (aguard-master 27.10 18:31 CET: старый cl не слал typed, сброс отклонён таймерами).
// claude остаётся дочерним (pid тот же), master pty — открытым дескриптором через exec. Под pty — только когда человек
// молчит humanQuiet: его ввод не пропадёт между двумя cl. tried — exec пробовали и он не вышел: строка в журнал, в этом
// запуске больше не пробовать.
func (l launcher) liveUpgrade(cfg config, now time.Time, pid, n int, args []string) (tried bool) {
	if !matchAny(cfg.Restart.LiveUpgradeNames, l.name) {
		return false
	}
	path, ok, err := replaced(l.selfExe)
	if !ok && err == nil {
		return false
	}
	if l.console != nil && l.console.idle(now) < humanQuiet {
		return false
	}
	back := func() {}
	if err == nil {
		u := upgrade{V: upgradeV, Name: l.name, N: n, Args: args, Pid: pid}
		if l.console != nil {
			u.Master, u.Typed, back, err = l.console.handOver()
		}
		if err == nil {
			err = l.reexec(cfg.Restart.Journal, path, u)
			back()
		}
	}
	if err == nil { // exec вернулся без ошибки — только подмена в тесте
		return false
	}
	journal(cfg.Restart.Journal, time.Now(), fmt.Sprintf("%s pid=%d: %v — дальше прежний cl", l.name, pid, err))
	return true
}

// adopt — живой claude от прежнего cl (liveUpgrade): дочерний процесс по pid и его pty в консоль. finish — как у launch.
func (l launcher) adopt(u upgrade) (*os.Process, func(), error) {
	proc, err := os.FindProcess(u.Pid)
	if err != nil {
		return nil, nil, fmt.Errorf("claude pid=%d от прежнего cl: %w", u.Pid, err)
	}
	if u.Master == 0 {
		return proc, func() {}, nil
	}
	master := os.NewFile(uintptr(u.Master), "pty claude")
	if l.console == nil { // терминал cl больше не терминал — вывод claude некуда девать
		return nil, nil, errors.Join(fmt.Errorf("pty claude pid=%d от прежнего cl: консоли нет", u.Pid), master.Close())
	}
	l.console.typed.Store(u.Typed)
	finish, err := l.console.attach(master, nil)
	if err != nil {
		return nil, nil, errors.Join(err, master.Close())
	}
	// экран перерисовать: кусок вывода, прочитанный прежним cl, мог не дойти до терминала
	if err := errors.Join(pty.InheritSize(l.console.in, master), proc.Signal(syscall.SIGWINCH)); err != nil {
		l.console.report("перерисовать экран", err)
	}
	return proc, finish, nil
}
