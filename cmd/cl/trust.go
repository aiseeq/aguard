package main

// Окно доверия к домашней папке (задание лида 27.10 22:0x CET по слову пользователя 22:00:36 CET в транскрипте лида
// 2dc3ff44: «Ладно, тогда настройку можно откатить, а cl может просто жать нужную кнопку»). Claude Code не сохраняет
// доверие к домашней папке («for security home-directory trust is never saved» — строка 2.1.311), и каждый подъём
// claude в ~ встаёт на окне «Accessing workspace»: 27.10 после сжатия перезапуском лид простоял на нём с 20:48 до
// ~21:45 CET, пока окно не увидел пользователь. Под pty cl видит вывод claude и выбирает «Yes, I trust this folder»
// сам: только в домашней папке (к остальным Claude Code доверие сохраняет после первого «да» человека — там окно
// значит новую папку, и смотреть её должен человек), только пока человек не нажимал клавиш, по разу на подъём.

import (
	"bytes"
	"fmt"
	"regexp"
	"time"
	"unicode"
)

const (
	// trustScanMax — сколько начала вывода claude смотреть: окно идёт первым экраном, до беседы (проба 27.10 —
	// первые 1,5 КБ).
	trustScanMax = 64 << 10
	// trustSettle — пауза после того, как окно появилось: меню должно успеть принять ввод.
	trustSettle = 500 * time.Millisecond
	// keyDown — стрелка вниз: в окне выбрано «No, exit», Enter сразу закрыл бы claude (проба в tmux 27.10 21:5x CET:
	// «2» выбор не меняет, стрелка вниз — меняет).
	keyDown = "\x1b[B"
)

// escSeq — управляющие последовательности терминала: CSI, OSC и двухбайтовые (ESC 7, ESC 8, ESC =).
var escSeq = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[^\\[\\]]")

// plainText — текст экрана без управляющих последовательностей и без пробелов: Claude Code рисует пробелы сдвигом
// курсора (CSI n C), и «Yes, I trust this folder» в выводе — «Yes,Itrustthisfolder».
func plainText(raw []byte) []byte {
	return bytes.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, escSeq.ReplaceAll(raw, nil))
}

// reply — ответы терминала на запросы claude и события фокуса: DCS (XTVERSION), OSC, APC (графика kitty), CSI с «?» на
// c, u, n и $y (DA1, флаги клавиатуры kitty, DSR, DECRPM), CPR, отчёты окна на t (XTWINOPS), фокус. Проба 27.10
// 22:01 CET под tmux: на подъёме claude ввод cl — «\x1bP>|tmux 3.7c\x1b\\», «\x1b[?1;2;4c»,
// «\x1b[?2026;2$y\x1b[?1;2;4c»; без этого окно доверия считалось занятым человеком. Замер 28.10 09:37 CET под Konsole
// 26.08.1 (там живут консоли агентов) с claude 2.1.312 — ещё «\x1b_Gi=31;OK\x1b\\» и «\x1b[6;28;13t»: без них лид
// простоял на окне с 00:38 до 09:33 CET.
var reply = regexp.MustCompile("\x1bP[^\x1b]*\x1b\\\\|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b_[^\x1b]*\x1b\\\\|\x1b\\[\\?[0-9;]*(?:[cun]|\\$y)|\x1b\\[[0-9;]*[Rnt]|\x1b\\[[IO]")

// terminalReplies — ввод целиком из ответов терминала: человек клавиш не нажимал.
func terminalReplies(p []byte) bool {
	return len(reply.ReplaceAll(p, nil)) == 0
}

// inputShapeMax — сколько байт ввода с ESC показывать в журнале: ответ терминала целиком, без хвоста вставки.
const inputShapeMax = 80

// inputShape — ввод, сдвинувший keyed, для журнала: без известных ответов терминала начинается с ESC — эти байты
// (неизвестный ответ терминала виден сразу, без новой пробы), иначе «клавиши»: набранный человеком текст в журнал
// не идёт.
func inputShape(p []byte) string {
	p = reply.ReplaceAll(p, nil)
	if len(p) == 0 || p[0] != 0x1b {
		return "клавиши"
	}
	return fmt.Sprintf("%q", p[:min(len(p), inputShapeMax)])
}

// keyedByShape — что последним сдвинуло keyed (inputShape).
func (c *console) keyedByShape() string {
	if by := c.keyedBy.Load(); by != nil {
		return *by
	}
	return "?"
}

// trustWatch смотрит начало вывода claude и один раз зовёт found, когда на экране окно доверия к папке dir.
type trustWatch struct {
	needles [][]byte
	buf     []byte
	done    bool
	found   func()
}

func newTrustWatch(dir string, found func()) *trustWatch {
	return &trustWatch{
		needles: [][]byte{[]byte("Accessingworkspace:"), plainText([]byte(dir)), []byte("Yes,Itrustthisfolder")},
		found:   found,
	}
}

// Write копит начало вывода; вывод claude в терминал от него не зависит — ошибок нет.
func (w *trustWatch) Write(p []byte) (int, error) {
	if w.done {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	text := plainText(w.buf)
	for _, n := range w.needles {
		if !bytes.Contains(text, n) {
			if len(w.buf) > trustScanMax {
				w.done, w.buf = true, nil
			}
			return len(p), nil
		}
	}
	w.done, w.buf = true, nil
	go w.found()
	return len(p), nil
}

// confirmTrust выбирает «Yes, I trust this folder» в окне доверия к dir. since — подъём claude: человек нажимал
// клавиши после него — окно его, cl не трогает.
func (c *console) confirmTrust(dir string, since int64) {
	time.Sleep(trustSettle)
	if c.keyed.Load() > since {
		c.say(fmt.Sprintf("окно доверия к %s: человек у терминала (ввод %s) — не подтверждаю", dir, c.keyedByShape()))
		return
	}
	if err := c.write(keyDown); err != nil {
		c.say(fmt.Sprintf("окно доверия к %s: %v", dir, err))
		return
	}
	time.Sleep(typePause)
	if c.keyed.Load() > since {
		c.say(fmt.Sprintf("окно доверия к %s: человек у терминала (ввод %s) — выбрал «да», Enter не нажимаю", dir,
			c.keyedByShape()))
		return
	}
	if err := c.write("\r"); err != nil {
		c.say(fmt.Sprintf("окно доверия к %s: %v", dir, err))
		return
	}
	c.say(fmt.Sprintf("подтвердил доверие к %s: Claude Code не сохраняет доверие к домашней папке", dir))
}
