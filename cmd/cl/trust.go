package main

// Окно доверия к домашней папке (задание лида 27.10 22:0x CET по слову пользователя 22:00:36 CET в транскрипте лида
// 2dc3ff44: «Ладно, тогда настройку можно откатить, а cl может просто жать нужную кнопку»). Claude Code не сохраняет
// доверие к домашней папке («for security home-directory trust is never saved» — строка 2.1.311), и каждый подъём
// claude в ~ встаёт на окне «Accessing workspace»: 27.10 после сжатия перезапуском лид простоял на нём с 20:48 до
// ~21:45 CET, пока окно не увидел пользователь. Под pty cl видит вывод claude и выбирает «Yes, I trust this folder»
// сам: только в домашней папке (к остальным Claude Code доверие сохраняет после первого «да» человека — там окно
// значит новую папку, и смотреть её должен человек), по разу на подъём. Условия «человек не нажимал клавиш» нет: по
// нему ночь 28.10 сорвалась — ответы Konsole на запросы claude cl принял за клавиши, и лид простоял на окне с 00:38
// до 09:33 CET (решение пользователя то же, `~/.claude/decisions.md` cl-trust-press-button; лид 28.10 10:25 CET).

import (
	"bytes"
	"regexp"
	"sync"
	"time"
	"unicode"

	"github.com/aiseeq/aguard/internal/cllog"
)

// trustSelectWait — сколько ждать подтверждения выбора «Yes»; переменная, чтобы тесты могли задать короткий срок.
var trustSelectWait = 10 * time.Second

const (
	// trustScanMax — сколько начала вывода claude смотреть: окно идёт первым экраном, до беседы (проба 27.10 —
	// 1,4 и 2,1 КБ; при --resume беседа дальше 8 КБ не должна влиять).
	trustScanMax = 8 << 10
	// trustSettle — пауза после того, как окно появилось: меню должно успеть принять ввод.
	trustSettle = 500 * time.Millisecond
	// keyDown — стрелка вниз: в окне выбрано «No, exit», Enter сразу закрыл бы claude (проба в tmux 27.10 21:5x CET:
	// «2» выбор не меняет, стрелка вниз — меняет).
	keyDown = "\x1b[B"
)

// Режим экранного чтеца (CLAUDE_AX_SCREEN_READER=1; проба 10.11 на 2.1.322, cl-trust-screen-reader): окно без «❯» —
// строки «y. Yes, I trust this folder», «n. No, exit» и поле «Enter y/n:»; выбор — клавиша y, Claude Code отвечает её
// эхом в поле (сдвиг курсора к колонке и « y»), Enter подтверждает. «y.» после сдвига — пункт меню при перерисовке, не эхо.
var (
	readerYes  = []byte("y.Yes,Itrustthisfolder")
	readerEcho = regexp.MustCompile(`\x1b\[[0-9]+G ?y([^.]|$)`)
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

// trustWatch смотрит начало вывода claude и один раз зовёт found, когда на экране окно доверия к папке dir.
type trustWatch struct {
	selectMu     sync.Mutex
	selectBuf    []byte
	selecting    bool
	selected     chan struct{}
	selectedOnce sync.Once
	needles      [][]byte
	buf          []byte
	done         bool
	reader       bool // окно в режиме экранного чтеца: выбор клавишей y
	found        func()
}

func newTrustWatch(dir string, found func()) *trustWatch {
	return &trustWatch{
		needles:  [][]byte{[]byte("Accessingworkspace:"), plainText([]byte(dir)), []byte("Yes,Itrustthisfolder")},
		found:    found,
		selected: make(chan struct{}),
	}
}

// Write копит начало вывода; вывод claude в терминал от него не зависит — ошибок нет.
func (w *trustWatch) Write(p []byte) (int, error) {
	w.selectMu.Lock()
	if w.selecting {
		w.selectBuf = append(w.selectBuf, p...)
		if w.reader && readerEcho.Match(w.selectBuf) || !w.reader && bytes.Contains(plainText(w.selectBuf), []byte("❯Yes,Itrustthisfolder")) {
			w.selecting = false
			w.selectedOnce.Do(func() { close(w.selected) })
		} else if len(w.selectBuf) > trustScanMax {
			w.selectBuf = append(w.selectBuf[:0], w.selectBuf[len(w.selectBuf)-trustScanMax:]...)
		}
	}
	w.selectMu.Unlock()
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
	w.selectMu.Lock()
	w.reader = bytes.Contains(text, readerYes)
	w.selectMu.Unlock()
	w.done, w.buf = true, nil
	go w.found()
	return len(p), nil
}

func (w *trustWatch) stopSelecting() {
	w.selectMu.Lock()
	w.selecting = false
	w.selectBuf = nil
	w.selectMu.Unlock()
}

// confirmTrust выбирает «Yes, I trust this folder» в окне доверия к dir.
func (c *console) confirmTrust(dir string, w *trustWatch) {
	time.Sleep(trustSettle)
	w.selectMu.Lock()
	w.selectBuf = nil
	w.selecting = true
	key := keyDown
	if w.reader {
		key = "y"
	}
	w.selectMu.Unlock()
	defer w.stopSelecting()
	if err := c.write(key); err != nil {
		c.say(cllog.TrustWriteFailed(dir, err))
		return
	}
	timer := time.NewTimer(trustSelectWait)
	defer timer.Stop()
	select {
	case <-w.selected:
	case <-timer.C:
		c.say(cllog.TrustNotSelected(dir, trustSelectWait))
		return
	}
	time.Sleep(typePause)
	if err := c.write("\r"); err != nil {
		c.say(cllog.TrustWriteFailed(dir, err))
		return
	}
	c.say(cllog.TrustConfirmed(dir))
}
