// Package screen — поле ввода Claude Code по тексту экрана терминала сессии: пустое, в нём черновик или не
// разобрать. В сессию шлют только то, что не затрёт набранное: перезапуск и /compact — только при пустом поле.
// Ввод в терминал (atime pts) черновик не ловит: текст, набранный во время хода, старше конца хода.
package screen

import (
	"strings"
	"unicode"
)

// Prompt — что в поле ввода.
type Prompt string

const (
	Empty   Prompt = "empty"
	Draft   Prompt = "draft"
	Unknown Prompt = "unknown" // рамки поля на экране нет: диалог, прокрутка, чужая программа
)

const (
	// statusLines — сколько строк под полем ввода бывает: строка статуса, режим, подсказки.
	statusLines = 8
	// inputLines — самое длинное поле, которое ищем: выше верхнюю линию уже не ищем.
	inputLines = 40
	// ruleLen — самая короткая линия рамки (в символах «─»): отличает рамку от «──» в тексте.
	ruleLen = 10
	marker  = "❯"
)

// View — что на экране сессии: поле ввода и подсказка Claude Code «new task? /clear to save 235.7k tokens» под ним.
// Подсказку Claude Code показывает сам у свободной сессии с большим контекстом — для него работа похожа на законченную.
type View struct {
	Prompt    Prompt
	ClearHint bool
}

// clearHint — текст подсказки сброса.
const clearHint = "/clear to save"

// Read — поле ввода и подсказка сброса по строкам экрана сверху вниз. Подсказку ищем в строках статуса под полем.
func Read(lines []string) View {
	v := View{Prompt: Parse(lines)}
	lines = trimBottom(lines)
	if bottom := bottomRule(lines); bottom >= 0 {
		for _, l := range lines[bottom+1:] {
			v.ClearHint = v.ClearHint || strings.Contains(l, clearHint)
		}
	}
	return v
}

// trimBottom — экран без пустых строк внизу.
func trimBottom(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// bottomRule — нижняя линия поля ввода: последняя линия рамки среди последних строк; -1 — нет.
func bottomRule(lines []string) int {
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-1-statusLines; i-- {
		if isRule(lines[i]) {
			return i
		}
	}
	return -1
}

// Parse — поле ввода по строкам экрана сверху вниз. Поле — рамка: верхняя линия из «─» (справа бывает имя сессии),
// первая строка с «❯», продолжения многострочного ввода и нижняя линия из одних «─»; под ней — только строки
// статуса. В новой сессии пустое поле показывает серый образец «Try "…"»: по тексту он неотличим от набранного и
// считается черновиком — лишний раз не тронуть дешевле, чем затереть.
func Parse(lines []string) Prompt {
	lines = trimBottom(lines)
	bottom := bottomRule(lines)
	if bottom < 1 {
		return Unknown
	}
	top := -1
	for i := bottom - 1; i >= 0 && i >= bottom-1-inputLines; i-- {
		if isTop(lines[i]) {
			top = i
			break
		}
	}
	if top < 0 || top+1 == bottom {
		return Unknown
	}
	first, ok := strings.CutPrefix(strings.TrimLeftFunc(lines[top+1], unicode.IsSpace), marker)
	if !ok {
		return Unknown
	}
	// unicode.IsSpace — и неразрывный пробел, которым Claude Code дописывает пустое поле
	for _, l := range append([]string{first}, lines[top+2:bottom]...) {
		if strings.TrimFunc(l, unicode.IsSpace) != "" {
			return Draft
		}
	}
	return Empty
}

// isRule — линия из одних «─», не короче ruleLen.
func isRule(l string) bool {
	l = strings.TrimSpace(l)
	return strings.Count(l, "─") >= ruleLen && strings.Trim(l, "─") == ""
}

// isTop — верхняя линия поля: начинается и кончается «─», между ними может стоять имя сессии.
func isTop(l string) bool {
	l = strings.TrimSpace(l)
	return strings.HasPrefix(l, strings.Repeat("─", ruleLen)) && strings.HasSuffix(l, "─")
}
