// Package said — указатель сказанного пользователем (задание лида 26.10, вариант А разбора
// ~/.claude/improve/runs/2026-10-26/said-lost.md; «да» пользователя 26.10 14:01:33 CET в транскрипте лида:
// «принимаю твою рекомендацию. Не должно же стоить дорого если гонять на луне?»). Каждую новую реплику
// человека в сессиях лида, админов и мастеров модель размечает: правило, решение, обещание или оперативная
// реплика; первые три дописываются в ~/.claude/said.tsv дословной цитатой. Это не журнал решений: «действует»
// не пишется, журнал ведёт Сон по «да» пользователя. Читают prior (akit), question-check (chooks) и Сон.
package said

import (
	_ "embed"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Prompt — инструкция модели; Schema — JSON Schema её ответа.
var (
	//go:embed prompt.md
	Prompt string
	//go:embed schema.json
	Schema string
)

// Режимы.
const (
	ModeOff   = "off"   // наблюдатель не работает
	ModeLog   = "log"   // разметка и расход — в журнал, said.tsv не пишется
	ModeWrite = "write" // и запись в said.tsv
)

// Config — раздел [said].
type Config struct {
	Mode     string          `toml:"mode"`
	Interval policy.Duration `toml:"interval"`
	// Names — сессии, чьи реплики размечаются (шаблоны path.Match по имени сессии).
	Names   []string        `toml:"names"`
	Model   string          `toml:"model"`  // имя из ~/.config/models/approved
	Effort  string          `toml:"effort"` // усилие рассуждения
	Timeout policy.Duration `toml:"timeout"`
	File    string          `toml:"file"` // указатель сказанного
	// Context — сколько знаков конца сообщения агента дать модели как «в ответ на».
	Context int `toml:"context"`
	// Candidates — сколько прежних записей той же области дать модели для поля supersedes.
	Candidates int `toml:"candidates"`
	Retries    int `toml:"retries"` // попыток на реплику, если модель не ответила
	Batch      int `toml:"batch"`   // реплик за проход; остальные ждут следующего
	// Pause — пауза после того, как модель не ответила (лимит, сбой мощности, сеть): один вызов за паузу.
	Pause policy.Duration `toml:"pause"`
	// Ceiling — used учётки OpenAI, с которого разметка её не берёт: остаток окна — бойцам.
	Ceiling float64 `toml:"ceiling"`
	// Stall — модель не отвечает столько подряд: письмо лиду, раз в сутки (указатель иначе молча стоит).
	Stall policy.Duration `toml:"stall"`
	Keep  policy.Duration `toml:"keep"` // срок журнала
}

// defaultContext — знаков конца сообщения агента: вопрос, на который отвечает реплика, обычно в последнем абзаце.
const defaultContext = 800

// Default — значения по умолчанию.
func Default() Config {
	return Config{
		Mode: ModeLog, Interval: policy.Duration(time.Minute), Names: []string{"lead", "*-admin", "*-master"},
		Model: "luna", Effort: "low", Timeout: policy.Duration(2 * time.Minute), File: "~/.claude/said.tsv",
		Context: defaultContext, Candidates: 8, Retries: 3, Batch: 20, Pause: policy.Duration(15 * time.Minute),
		Ceiling: 90, Stall: policy.Duration(6 * time.Hour), Keep: policy.Duration(60 * 24 * time.Hour),
	}
}

// Validate раскрывает ~ и проверяет значения.
func (c *Config) Validate(home string) error {
	switch c.Mode {
	case ModeOff, ModeLog, ModeWrite:
	default:
		return fmt.Errorf("said.mode = %q: off, log или write", c.Mode)
	}
	if c.Interval.D() < 10*time.Second || c.Timeout.D() < 10*time.Second || c.Pause.D() < c.Interval.D() ||
		c.Keep.D() < 24*time.Hour {
		return errors.New("said: interval и timeout от 10s, pause не меньше interval, keep от суток")
	}
	if c.Ceiling <= 0 || c.Ceiling > 100 || c.Stall.D() < c.Pause.D() {
		return errors.New("said.ceiling в (0, 100], said.stall >= said.pause")
	}
	if c.Model == "" || c.File == "" || c.Context < 0 || c.Candidates < 0 || c.Retries < 1 || c.Batch < 1 {
		return errors.New("said: нужны model и file, context и candidates не меньше 0, retries и batch от 1")
	}
	for _, p := range c.Names {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("said.names %q: %w", p, err)
		}
	}
	if rest, ok := strings.CutPrefix(c.File, "~/"); ok {
		c.File = home + "/" + rest
	}
	return nil
}

// Watched — реплики этой сессии размечаются.
func (c *Config) Watched(name string) bool {
	for _, p := range c.Names {
		if ok, err := path.Match(p, name); err == nil && ok && name != "" { // шаблоны проверил Validate
			return true
		}
	}
	return false
}
