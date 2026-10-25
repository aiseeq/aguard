package compact

import (
	"math"
	"strings"
)

// price — цена модели за миллион токенов: вход и чтение кэша. Запись кэша на час (её пишет Claude Code) — 2× входа.
// Источник — справка claude-api (таблица моделей от 2026-10-06, prompt-caching.md «Economics»), не счёт: оценка.
type price struct {
	input, read float64
}

var prices = map[string]price{
	"claude-fable-5-1":  {10, 0.25},
	"claude-fable-5":    {10, 1},
	"claude-opus-5-5":   {4, 0.20},
	"claude-opus-5":     {5, 0.50},
	"claude-opus-4-8":   {5, 0.50},
	"claude-sonnet-5-5": {2, 0.20},
	"claude-sonnet-5":   {2, 0.20},
	"claude-haiku-5-5":  {0.10, 0.01},
}

// cost — во что обойдётся контекст tokens модели model, в миллионных долях доллара: rewrite — следующий запрос после
// смерти кэша пишет его заново, read — каждый запрос при живом кэше читает его. Цена за миллион токенов в долларах —
// это миллионные доли доллара за токен. Модель не из таблицы — нули.
func cost(model string, tokens int64) (rewrite, read int64) {
	p, ok := prices[strings.TrimSuffix(model, "[1m]")]
	if !ok {
		return 0, 0
	}
	t := float64(tokens)
	return int64(math.Round(2 * p.input * t)), int64(math.Round(p.read * t))
}
