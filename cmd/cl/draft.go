package main

// Набран ли в поле ввода claude текст человека: cl видит весь его ввод (случай chooks-master 30.10 — серая подсказка
// Claude Code в поле держала сжатие и перезапуск каждой простаивающей сессии, которую ведут письма). Ошибка в любую
// сторону, кроме «текст есть, а cl считает поле чистым», безопасна: чистым поле становится только после отправки.

import "bytes"

const (
	pasteEnd = "\x1b[201~"
	keyEsc   = 0x1b
	keyEnter = '\r'
	keyCtrlC = 0x03
	keyTab   = '\t'
)

// draftAfter — может ли в поле быть набранный текст после ввода b, если до него могло (was). Enter отправляет
// реплику, Ctrl+C очищает поле — чисто; печатные знаки, Tab (дополнение) и стрелки вверх-вниз (история) — текст.
// Вставка (\e[200~ … \e[201~) — текст целиком, её \r не отправка; \ или Esc перед \r — перевод строки, не отправка.
// Ответы терминала, фокус окна, мышь и прочие последовательности поле не меняют. Последовательность, разрезанная
// между чтениями, читается как текст — в безопасную сторону.
func draftAfter(was bool, b []byte) bool {
	d := was
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == keyEsc:
			n, text, paste := escape(b[i:])
			if paste {
				end := bytes.Index(b[i+n:], []byte(pasteEnd))
				if end < 0 {
					return true
				}
				n += end + len(pasteEnd)
				text = true
			}
			if text {
				d = true
			}
			i += n - 1
		case c == keyEnter:
			if i > 0 && (b[i-1] == '\\' || b[i-1] == keyEsc) {
				d = true
				continue
			}
			d = false
		case c == keyCtrlC:
			d = false
		case c == keyTab || c >= 0x20 && c != 0x7f:
			d = true
		}
	}
	return d
}

// escape — длина последовательности с Esc в начале b; text — она меняет поле (стрелка вверх или вниз), paste —
// начало вставки. Одинокий Esc и Esc перед \r — длина 1: \r разберёт вызывающий.
func escape(b []byte) (n int, text, paste bool) {
	if len(b) < 2 {
		return 1, false, false
	}
	switch b[1] {
	case '[':
		return csi(b)
	case 'O': // SS3: F1–F4, стрелки в режиме приложения
		if len(b) < 3 {
			return 2, true, false
		}
		return 3, b[2] == 'A' || b[2] == 'B', false
	case ']':
		return osc(b), false, false
	case keyEnter:
		return 1, false, false
	}
	return 2, b[1] >= 0x20 && b[1] != 0x7f, false // Alt+знак — знак в поле
}

// csi — параметры 0x30–0x3F, промежуточные 0x20–0x2F, финальный 0x40–0x7E; стрелки вверх-вниз — история в поле.
func csi(b []byte) (n int, text, paste bool) {
	j := 2
	for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
		j++
	}
	if j == len(b) {
		return j, true, false
	}
	final := b[j]
	if string(b[2:j]) == "200" && final == '~' {
		return j + 1, false, true
	}
	return j + 1, final == 'A' || final == 'B', false
}

// osc — ответ терминала до BEL или ST.
func osc(b []byte) int {
	for j := 2; j < len(b); j++ {
		if b[j] == 0x07 {
			return j + 1
		}
		if b[j] == keyEsc && j+1 < len(b) && b[j+1] == '\\' {
			return j + 2
		}
	}
	return len(b)
}
