package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/night"
)

// fallbackRun — запасной Сон за дату (night.dream_fallback): запускается один раз, даже если упал; конец unit —
// второй сигнал с кодом и хвостом журнала.
type fallbackRun struct {
	Unit    string    `json:"unit"`
	At      time.Time `json:"at"`
	Err     string    `json:"err,omitempty"` // systemd-run не запустил
	Done    bool      `json:"done,omitempty"`
	Lost    bool      `json:"lost,omitempty"` // unit пропал из systemd (перезагрузка, снят руками): кода нет
	Code    int       `json:"code,omitempty"`
	Result  string    `json:"result,omitempty"`
	Tail    string    `json:"tail,omitempty"`
	EndedAt time.Time `json:"ended_at,omitzero"`
}

// fallbackState — запуски по местной дате.
type fallbackState map[string]fallbackRun

// unitStatus — состояние unit по systemctl show; код — ExecMainStatus.
type unitStatus struct {
	Load, Active, Sub, Result string
	Code                      int
}

// lost — unit нет в systemd (перезагрузка, снят руками): show отдаёт для него inactive, success и код 0, а это не конец
// команды (проверка git-admin 30.10).
func (s unitStatus) lost() bool { return s.Load == "not-found" }

// finished — команда кончилась: с RemainAfterExit unit остаётся active/exited, упавший — failed; пропавший — тоже конец,
// но без кода.
func (s unitStatus) finished() bool {
	return s.lost() || s.Sub == "exited" || s.Active == "failed" || s.Active == "inactive"
}

// unitRunner — запуск команды отдельным unit пользователя и наблюдение за ним; в тестах — подмена.
type unitRunner interface {
	start(unit, dir, cmd string) error
	status(unit string) (unitStatus, error)
	tail(unit string) string
	clean(unit string) error
}

// systemdUnits — systemd-run --user: команда живёт отдельно от службы aguard (её перезапуск при `graft install` её не
// трогает), bash -l — PATH и окружение профиля, как в консоли.
type systemdUnits struct{}

func (systemdUnits) start(unit, dir, cmd string) error {
	out, err := exec.Command("systemd-run", "--user", "--unit="+unit, "--working-directory="+dir,
		"-p", "RemainAfterExit=yes", "--", "bash", "-lc", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (systemdUnits) status(unit string) (unitStatus, error) {
	out, err := exec.Command("systemctl", "--user", "show", unit, "-p", "LoadState", "-p", "ActiveState", "-p", "SubState",
		"-p", "Result", "-p", "ExecMainStatus").Output()
	if err != nil {
		return unitStatus{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	var s unitStatus
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "LoadState":
			s.Load = value
		case "ActiveState":
			s.Active = value
		case "SubState":
			s.Sub = value
		case "Result":
			s.Result = value
		case "ExecMainStatus":
			if s.Code, err = strconv.Atoi(value); err != nil {
				return s, fmt.Errorf("systemctl show %s: ExecMainStatus %q: %w", unit, value, err)
			}
		}
	}
	return s, nil
}

func (systemdUnits) tail(unit string) string {
	out, err := exec.Command("journalctl", "--user", "-u", unit, "-n", "12", "-o", "cat", "--no-pager").Output()
	if err != nil {
		return fmt.Sprintf("журнал unit не прочитан: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// clean снимает кончившийся unit: имя за дату больше не нужно, а active/exited держал бы его до перезагрузки. Уже
// выгруженный (перезагрузка, снят руками) — не ошибка.
func (systemdUnits) clean(unit string) error {
	for _, verb := range []string{"stop", "reset-failed"} {
		out, err := exec.Command("systemctl", "--user", verb, unit).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "not loaded") {
			return fmt.Errorf("systemctl %s %s: %w: %s", verb, unit, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// fallbackRound — запустить запасной Сон, если пора, и проверить идущие; signals — по каждому запуску за сутки (доставку
// и повтор неудачной ведёт dispatchNight по ключу). save — состояние на диск до запуска: упавший демон не запустит
// второй раз; не записалось — запуска нет, письмо о том, что запускать руками. Запуск и конец unit — тоже на диск;
// сбой этой записи — заметка: в памяти верно, а после перезапуска демона конец придёт снова под тем же ключом письма.
// log — mode=log: только «запустил бы».
func fallbackRound(cfg *night.Config, now time.Time, facts night.Facts, state fallbackState, runner unitRunner, dir string,
	save func(fallbackState) error, log bool) ([]night.Signal, []string) {
	date := now.Format("2006-01-02")
	var notes []string
	if _, launched := state[date]; !launched && night.FallbackDue(now, *cfg, facts) {
		unit := night.FallbackUnit(now)
		if log {
			return nil, []string{fmt.Sprintf("запустил бы %s: %s", unit, cfg.DreamFallback)}
		}
		state[date] = fallbackRun{Unit: unit, At: now}
		if err := save(state); err != nil {
			delete(state, date)
			return []night.Signal{{Key: "dream-fallback-blocked:" + date, To: cfg.DreamTo, Text: fmt.Sprintf(
					"Сон не стартовал к %s (нет маркера), а запасной на Terra я пока не запустил: не записывается состояние (%v) — без "+
						"записи запуск мог бы повториться. Пробую каждую минуту и запущу сам, как только запись пройдёт (до %s); не ждёшь — "+
						"запусти `%s` сам: при начатом Сне (started или тикет dream-run) он выходит, второго прогона не будет.",
					cfg.DreamFallbackBy, err, night.FallbackDeadline(*cfg), cfg.DreamFallback)}},
				[]string{"не запустил " + unit + ": " + err.Error()}
		}
		rec := state[date]
		if err := runner.start(unit, dir, cfg.DreamFallback); err != nil {
			rec.Err = err.Error()
		}
		state[date] = rec
		notes = append(notes, fmt.Sprintf("запустил %s: %s (%s)", unit, cfg.DreamFallback, orDash(rec.Err)))
	}
	var signals []night.Signal
	for _, d := range slices.Sorted(maps.Keys(state)) {
		rec := state[d]
		if rec.Err == "" && !rec.Done {
			st, err := runner.status(rec.Unit)
			if err != nil {
				notes = append(notes, err.Error())
			} else if st.finished() {
				rec.Done, rec.Lost, rec.Code, rec.Result, rec.EndedAt = true, st.lost(), st.Code, st.Result, now
				rec.Tail = runner.tail(rec.Unit)
				if rec.Lost {
					rec.Code, rec.Result = 0, ""
				} else if err := runner.clean(rec.Unit); err != nil {
					notes = append(notes, err.Error())
				}
				state[d] = rec
				notes = append(notes, fmt.Sprintf("%s кончился: код %d, %s, пропал %v", rec.Unit, rec.Code, rec.Result, rec.Lost))
			}
		}
		if now.Sub(rec.At) <= 24*time.Hour {
			for _, s := range fallbackSignals(cfg, d, rec) {
				// «запуск не вышел, запусти сам» после окна уже ничего не спасёт; отчёты о прошедшем запуске — сутки
				if rec.Err != "" && s.Key == "dream-fallback:"+d && !now.Before(night.FallbackEndAt(rec.At, *cfg)) {
					continue
				}
				signals = append(signals, s)
			}
		}
	}
	if len(notes) > 0 {
		if err := save(state); err != nil {
			notes = append(notes, "состояние не записано: "+err.Error())
		}
	}
	return signals, notes
}

// fallbackSignals — «Сон запущен на Terra» (или не запустился) и, когда кончился, — код и хвост журнала.
func fallbackSignals(cfg *night.Config, date string, rec fallbackRun) []night.Signal {
	text := fmt.Sprintf("Сон не стартовал к %s (нет маркера) — запустил на Terra: `%s`, unit %s. Смотреть: systemctl --user status %s; "+
		"journalctl --user -u %s. Повторно за %s не запущу.", cfg.DreamFallbackBy, cfg.DreamFallback, rec.Unit, rec.Unit, rec.Unit, date)
	if rec.Err != "" {
		text = fmt.Sprintf("Сон не стартовал к %s (нет маркера), а запасной запуск на Terra не вышел: %s. Повторно за %s не запущу — "+
			"запусти сам: `%s`.", cfg.DreamFallbackBy, rec.Err, date, cfg.DreamFallback)
	}
	out := []night.Signal{{Key: "dream-fallback:" + date, To: cfg.DreamTo, Text: text}}
	switch {
	case rec.Lost:
		out = append(out, night.Signal{Key: "dream-fallback-end:" + date, To: cfg.DreamTo, Text: fmt.Sprintf(
			"Сон на Terra (%s) пропал из systemd к %s (перезагрузка или снят руками) — кода нет, кончился ли он, не знаю. "+
				"Хвост журнала:\n%s", rec.Unit, rec.EndedAt.Format("15:04"), rec.Tail)})
	case rec.Done:
		out = append(out, night.Signal{Key: "dream-fallback-end:" + date, To: cfg.DreamTo, Text: fmt.Sprintf(
			"Сон на Terra (%s) кончился в %s: код %d, %s. Хвост журнала:\n%s", rec.Unit, rec.EndedAt.Format("15:04"), rec.Code,
			rec.Result, rec.Tail)})
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "ok"
	}
	return s
}

func loadFallbackState(path string) (fallbackState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fallbackState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("запасной Сон: %w", err)
	}
	state := fallbackState{}
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("запасной Сон %s: %w", path, err)
	}
	return state, nil
}

func saveFallbackState(path string, state fallbackState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("запасной Сон: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("запасной Сон: %w", err)
	}
	// атомарно: обрезанный при сбое файл не читается, а без чтения шаг не запускает ничего
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("запасной Сон: %w", err)
	}
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), path)
	}
	if werr != nil {
		if rerr := os.Remove(f.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			werr = errors.Join(werr, rerr)
		}
		return fmt.Errorf("запасной Сон: %w", werr)
	}
	return nil
}
