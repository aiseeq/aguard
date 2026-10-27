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
	"time"
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
	journal(journalFile, time.Now(), fmt.Sprintf("%s pid=%d: cl обновлён — перехожу в новый бинарник %s", u.Name, os.Getpid(), path))
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
