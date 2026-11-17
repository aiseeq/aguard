package main

// Claude Code не сохраняет доверие к домашней папке («for security home-directory trust is never saved»), поэтому
// cl распознаёт её окно вместе с окнами своих --add-dir. Окна наблюдаются только до первого баннера REPL.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/aguard/internal/cllog"
)

// trustSelectWait — сколько ждать подтверждения выбора «Yes»; переменная, чтобы тесты могли задать короткий срок.
var trustSelectWait = 10 * time.Second

// trustRetryPause — задержка перед повторным Down после явной подсказки Claude Code.
var trustRetryPause = time.Second

const (
	trustScanMax        = 64 << 10 // Широкие терминалы могут вывести несколько окон до баннера REPL.
	trustSettle         = 500 * time.Millisecond
	keyDown             = "\x1b[B" // По умолчанию выбрано «No, exit».
	trustCursorGap byte = 1        // курсорный отступ может быть пробелом или переносом строки
)

var (
	readerEcho = regexp.MustCompile(`\x1b\[[0-9]+G ?y[\x1b\r\n]`)
	escSeq     = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[^\\[\\]]")
)

type trustWindow struct {
	dir       string
	yes       string
	key       string // home, add-dir или foreign
	reader    bool
	ambiguous bool
	wrapped   bool
	addDir    bool
	blocked   bool
}

type trustPathError func(string, error)

type trustWatchCallbacks struct {
	pathError   trustPathError
	scanLimit   func(int)
	blockAddDir bool
}

// trustWatch распознаёт диалоги в начальном выводе и подтверждает их через последовательную очередь.
type trustWatch struct {
	selectMu       sync.Mutex
	selectBuf      []byte
	selecting      bool
	selectionEvent chan trustSelectionEvent // отдельное ожидание для каждого нажатия
	selectedNeedle []byte
	buf            []byte
	scanDone       bool
	homeDir        string
	addDirs        map[string]bool
	seen           map[string]bool
	windows        chan trustWindow
	stop           chan struct{}
	workerDone     chan struct{}
	stopOnce       sync.Once
	onWindow       func(trustWindow)
	pathError      trustPathError
	scanLimit      func(int)
	blockAddDir    bool
}

type trustSelectionEvent uint8

const (
	trustSelectionRedrawn trustSelectionEvent = iota + 1
	trustSelectionPrompt
)

func newTrustWatch(home string, addDirs []string, onWindow func(trustWindow)) *trustWatch {
	return newTrustWatchWithCallbacks(home, addDirs, onWindow, trustWatchCallbacks{})
}

func newTrustWatchWithCallbacks(home string, addDirs []string, onWindow func(trustWindow), callback trustWatchCallbacks) *trustWatch {
	w := &trustWatch{
		homeDir:     cleanTrustDir(home, home),
		pathError:   callback.pathError,
		scanLimit:   callback.scanLimit,
		blockAddDir: callback.blockAddDir,
		seen:        make(map[string]bool),
		windows:     make(chan trustWindow, 128),
		stop:        make(chan struct{}),
		workerDone:  make(chan struct{}),
		onWindow:    onWindow,
	}
	w.addDirs = trustDirSetWithError(addDirs, home, w.pathError)
	go w.run()
	return w
}

func (w *trustWatch) run() {
	defer close(w.workerDone)
	for {
		select {
		case <-w.stop:
			return
		case win := <-w.windows:
			w.onWindow(win)
		}
	}
}

func (w *trustWatch) close() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.workerDone
}

func cleanTrustDir(dir, home string) string {
	if dir == "~" {
		return filepath.Clean(home)
	}
	if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, dir[2:])
	}
	return filepath.Clean(dir)
}

func trustDirKeys(dir, home string, report trustPathError) []string {
	dir = cleanTrustDir(dir, home)
	keys := []string{dir}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if !os.IsNotExist(err) && report != nil {
			report(dir, err)
		}
		return keys
	}
	resolved = filepath.Clean(resolved)
	if resolved != dir {
		keys = append(keys, resolved)
	}
	return keys
}

func trustDirSet(dirs []string, home string) map[string]bool {
	return trustDirSetWithError(dirs, home, nil)
}

func trustDirSetWithError(dirs []string, home string, report trustPathError) map[string]bool {
	set := make(map[string]bool, len(dirs)*2)
	for _, dir := range dirs {
		for _, key := range trustDirKeys(dir, home, report) {
			set[key] = true
		}
	}
	return set
}

func firstTrustPathError(report []trustPathError) trustPathError {
	if len(report) == 0 {
		return nil
	}
	return report[0]
}

// Write наблюдает только за начальным экраном; вывод Claude Code напрямую проходит в терминал cl.
func (w *trustWatch) Write(p []byte) (int, error) {
	w.selectMu.Lock()
	if w.selecting && w.selectionEvent != nil {
		w.selectBuf = append(w.selectBuf, p...)
		var event trustSelectionEvent
		compact := compactText(escSeq.ReplaceAll(w.selectBuf, nil))
		if readerEcho.Match(w.selectBuf) || bytes.Contains(compact, w.selectedNeedle) || bytes.Contains(w.selectBuf, []byte("\x1b[1B❯")) {
			event = trustSelectionRedrawn
		} else if trustRetryPrompt(compact) {
			event = trustSelectionPrompt
		}
		if event != 0 {
			w.selecting = false
			w.selectionEvent <- event
		} else if len(w.selectBuf) > trustScanMax {
			w.selectBuf = append(w.selectBuf[:0], w.selectBuf[len(w.selectBuf)-trustScanMax:]...)
		}
	}
	w.selectMu.Unlock()

	if w.scanDone {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	scan := w.buf
	if len(scan) > trustScanMax {
		scan = scan[:trustScanMax]
	}
	windows, complete := scanTrustWindows(scan, w.homeDir, w.addDirs, w.pathError)
	w.queueWindows(windows)
	limitReached := len(w.buf) >= trustScanMax
	if complete || limitReached {
		w.scanDone = true
		w.buf = nil
		if limitReached && !complete && w.scanLimit != nil {
			w.scanLimit(trustScanMax)
		}
	}
	return len(p), nil
}

func (w *trustWatch) queueWindows(windows []trustWindow) {
	for _, win := range windows {
		win.blocked = win.addDir && w.blockAddDir
		key := win.key + "\x00" + win.dir
		if w.seen[key] {
			continue
		}
		w.seen[key] = true
		select {
		case w.windows <- win:
		case <-w.stop:
		}
	}
}

// trustRetryPrompt matches the distinctive retry instruction. Claude may redraw it
// as a screen diff and leave the initial "E" of "Esc" from the prior frame in place.
func trustRetryPrompt(compact []byte) bool {
	return bytes.Contains(compact, compactText([]byte("To add it, press ↓ to Yes, then Enter")))
}

// compactText removes ANSI and whitespace while mapping each compact byte back to its source offset.
func compactText(raw []byte) []byte {
	compact, _ := compactOffsets(raw)
	return compact
}

func compactOffsets(raw []byte) ([]byte, []int) {
	var compact []byte
	var offsets []int
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if !unicode.IsSpace(r) && r != rune(trustCursorGap) {
			compact = append(compact, raw[i:i+size]...)
			for range size {
				offsets = append(offsets, i)
			}
		}
		i += size
	}
	return compact, offsets
}

// trustScreenText removes terminal controls while retaining horizontal gaps that separate path words.
func trustScreenText(raw []byte) []byte {
	b := trustScreenBuilder{column: 1, columnKnown: true}
	for i := 0; i < len(raw); {
		if raw[i] == '\r' || raw[i] == '\n' {
			b.newLine(raw[i])
			i++
			continue
		}
		if end, ok := b.consumeCSI(raw, i); ok {
			i = end
			continue
		}
		if end, ok := consumeTrustEscape(raw, i); ok {
			i = end
			continue
		}
		i = b.appendRune(raw, i)
	}
	return b.text
}

type trustScreenBuilder struct {
	text        []byte
	column      int
	lineHasText bool
	columnKnown bool
}

func (b *trustScreenBuilder) consumeCSI(raw []byte, start int) (int, bool) {
	if start+1 >= len(raw) || raw[start] != '\x1b' || raw[start+1] != '[' {
		return start, false
	}
	end := start + 2
	for end < len(raw) && (raw[end] < 0x40 || raw[end] > 0x7e) {
		end++
	}
	if end == len(raw) {
		return start, false
	}
	b.applyCSI(raw[start+2:end], raw[end])
	return end + 1, true
}

func (b *trustScreenBuilder) applyCSI(params []byte, final byte) {
	switch final {
	case 'G':
		target, valid := trustCSIParam(params, 1)
		if !valid {
			b.markAmbiguous()
			return
		}
		b.setColumn(target)
	case 'C':
		step, valid := trustCSIParam(params, 1)
		if !valid {
			b.markAmbiguous()
			return
		}
		b.moveRight(step)
	case 'A', 'B', 'D', 'E', 'F', 'H', 'f', 'd':
		b.markAmbiguous()
	}
}

func (b *trustScreenBuilder) setColumn(target int) {
	if !b.lineHasText {
		b.column, b.columnKnown = target, true
		return
	}
	if !b.columnKnown || target < b.column {
		b.text = append(b.text, 0)
	} else if target > b.column {
		b.text = appendCursorGap(b.text)
	}
	b.column, b.columnKnown = target, true
}

func (b *trustScreenBuilder) moveRight(step int) {
	if b.lineHasText && step > 0 {
		b.text = appendCursorGap(b.text)
	}
	b.column += step
}

func (b *trustScreenBuilder) markAmbiguous() {
	if b.lineHasText {
		b.text = append(b.text, 0)
		b.columnKnown = false
	}
}

func (b *trustScreenBuilder) newLine(char byte) {
	b.text = append(b.text, char)
	b.column, b.lineHasText, b.columnKnown = 1, false, true
}

func (b *trustScreenBuilder) appendRune(raw []byte, start int) int {
	r, size := utf8.DecodeRune(raw[start:])
	b.text = append(b.text, raw[start:start+size]...)
	b.lineHasText = b.lineHasText || !unicode.IsSpace(r)
	if r > 0x7f {
		b.columnKnown = false
	} else {
		b.column += size
	}
	return start + size
}

func consumeTrustEscape(raw []byte, start int) (int, bool) {
	if start >= len(raw) || raw[start] != '\x1b' {
		return start, false
	}
	if seq := escSeq.FindIndex(raw[start:]); seq != nil && seq[0] == 0 {
		return start + seq[1], true
	}
	return start, false
}

func trustCSIParam(params []byte, fallback int) (int, bool) {
	field := bytes.TrimLeft(params, "?><!")
	if end := bytes.IndexByte(field, ';'); end >= 0 {
		field = field[:end]
	}
	if len(field) == 0 {
		return fallback, true
	}
	value, err := strconv.Atoi(string(field))
	if value == 0 {
		value = fallback
	}
	return value, err == nil
}

func lastIsSpace(text []byte) bool {
	return len(text) > 0 && (text[len(text)-1] == ' ' || text[len(text)-1] == '\t')
}

func appendCursorGap(text []byte) []byte {
	if lastIsSpace(text) || len(text) > 0 && text[len(text)-1] == trustCursorGap {
		return text
	}
	return append(text, trustCursorGap)
}

var trustLabels = []struct {
	label  []byte
	marker []byte
	yes    []byte
	key    string
}{
	{[]byte("Accessingworkspace:"), []byte("Quicksafetycheck"), []byte("Yes,Itrustthisfolder"), "home"},
	{[]byte("Addinganewdirectory:"), []byte("Thissessionhasn'tworkedherebefore"), []byte("Yes,addit"), "add-dir"},
}

// scanTrustWindows binds each path to its dialog marker and menu, and stops at the first REPL banner.
func scanTrustWindows(raw []byte, home string, addDirs map[string]bool, report ...trustPathError) ([]trustWindow, bool) {
	clean := trustScreenText(raw)
	compact, offsets := compactOffsets(clean)
	lower := bytes.ToLower(compact)
	end := len(compact)
	complete := false
	if banner := bytes.Index(lower, []byte("claudecodev")); banner >= 0 {
		end, complete = banner, true
	}
	var out []trustWindow
	for _, pattern := range trustLabels {
		for from := 0; from < end; {
			i := bytes.Index(lower[from:end], bytes.ToLower(pattern.label))
			if i < 0 {
				break
			}
			start := from + i
			win, next, ok := parseTrustWindow(clean, lower, offsets, pattern, start, end, home, addDirs, report...)
			if ok {
				out = append(out, win)
			}
			from = next
		}
	}
	return out, complete
}

func parseTrustWindow(clean, lower []byte, offsets []int, pattern struct {
	label  []byte
	marker []byte
	yes    []byte
	key    string
}, start, end int, home string, addDirs map[string]bool, report ...trustPathError) (trustWindow, int, bool) {
	label := bytes.ToLower(pattern.label)
	labelEnd := start + len(label)
	limit := end
	if nextLabel := nextTrustLabel(lower[labelEnd:end]); nextLabel >= 0 {
		limit = labelEnd + nextLabel
	}
	next := limit
	markerOffset := bytes.Index(lower[labelEnd:limit], bytes.ToLower(pattern.marker))
	if markerOffset < 0 || labelEnd >= len(offsets) {
		return trustWindow{}, next, false
	}
	marker := labelEnd + markerOffset
	if marker >= limit || !bytes.Contains(lower[marker:limit], bytes.ToLower(pattern.yes)) {
		return trustWindow{}, next, false
	}
	pathBytes := clean[offsets[labelEnd]:offsets[marker]]
	multilinePath := trustPathHasMultipleLines(pathBytes)
	ambiguous := bytes.Contains(pathBytes, []byte{0}) || multilinePath
	pathBytes = bytes.ReplaceAll(pathBytes, []byte{0}, nil)
	pathBytes = bytes.ReplaceAll(pathBytes, []byte("\r"), nil)
	pathBytes = bytes.ReplaceAll(pathBytes, []byte("\n"), nil)
	if multilinePath {
		pathBytes = bytes.ReplaceAll(pathBytes, []byte{trustCursorGap}, nil)
	} else {
		pathBytes = bytes.ReplaceAll(pathBytes, []byte{trustCursorGap}, []byte{' '})
	}
	dir := cleanTrustDir(string(pathBytes), home)
	if dir == "." || dir == "" {
		return trustWindow{}, next, false
	}
	win := trustWindow{
		dir: dir, key: pattern.key, reader: bytes.Contains(lower[marker:limit], []byte("entery/n:")),
		ambiguous: ambiguous, wrapped: multilinePath, addDir: pattern.key == "add-dir",
	}
	if ambiguous {
		win.key = "foreign"
		return win, next, true
	}
	switch pattern.key {
	case "home":
		if !sameTrustDirWithError(dir, home, home, firstTrustPathError(report)) {
			return trustWindow{}, next, false
		}
		win.yes = "Yes, I trust this folder"
	case "add-dir":
		if !trustDirMatchesWithError(dir, home, addDirs, firstTrustPathError(report)) {
			win.key = "foreign"
			return win, next, true
		}
		win.yes = "Yes, add it"
	}
	return win, next, true
}

func trustPathHasMultipleLines(path []byte) bool {
	nonEmpty := 0
	for line := range bytes.SplitSeq(path, []byte{'\n'}) {
		line = bytes.ReplaceAll(line, []byte{0}, nil)
		line = bytes.ReplaceAll(line, []byte{trustCursorGap}, nil)
		line = bytes.ReplaceAll(line, []byte{'\r'}, nil)
		if len(bytes.TrimSpace(line)) > 0 {
			nonEmpty++
		}
	}
	return nonEmpty > 1
}

func nextTrustLabel(lower []byte) int {
	next := -1
	for _, label := range [][]byte{bytes.ToLower(trustLabels[0].label), bytes.ToLower(trustLabels[1].label)} {
		if at := bytes.Index(lower, label); at >= 0 && (next < 0 || at < next) {
			next = at
		}
	}
	return next
}

func sameTrustDirWithError(a, b, home string, report trustPathError) bool {
	keys := trustDirSetWithError([]string{a}, home, report)
	for _, key := range trustDirKeys(b, home, report) {
		if keys[key] {
			return true
		}
	}
	return false
}

func trustDirMatchesWithError(dir, home string, allowed map[string]bool, report trustPathError) bool {
	for _, key := range trustDirKeys(dir, home, report) {
		if allowed[key] {
			return true
		}
	}
	return false
}

func (w *trustWatch) stopSelecting(event chan trustSelectionEvent) {
	w.selectMu.Lock()
	if w.selectionEvent == event {
		w.selecting = false
		w.selectionEvent = nil
		w.selectBuf = nil
	}
	w.selectMu.Unlock()
}

func (c *console) confirmTrust(win trustWindow, w *trustWatch) {
	dir := win.dir
	timer := time.NewTimer(trustSettle)
	select {
	case <-timer.C:
	case <-w.stop:
		timer.Stop()
		return
	}
	deadline := time.NewTimer(trustSelectWait)
	defer deadline.Stop()
	key := keyDown
	if win.reader {
		key = "y"
	}
	for retries := 0; ; {
		event := make(chan trustSelectionEvent, 1)
		w.selectMu.Lock()
		w.selectionEvent = event
		w.selectBuf = nil
		w.selecting = true
		w.selectedNeedle = compactText([]byte("❯" + win.yes))
		w.selectMu.Unlock()
		if err := c.write(key); err != nil {
			w.stopSelecting(event)
			c.say(cllog.TrustWriteFailed(dir, err))
			return
		}
		if retries > 0 {
			c.say(cllog.TrustSelectionRetry(dir, retries))
		}
		select {
		case result := <-event:
			w.stopSelecting(event)
			if result == trustSelectionRedrawn {
				goto selected
			}
			if retries >= 3 {
				c.say(cllog.TrustNotSelected(dir, trustSelectWait))
				return
			}
			retry := time.NewTimer(trustRetryPause)
			select {
			case <-retry.C:
			case <-deadline.C:
				retry.Stop()
				c.say(cllog.TrustNotSelected(dir, trustSelectWait))
				return
			case <-w.stop:
				retry.Stop()
				return
			}
			retries++
		case <-deadline.C:
			w.stopSelecting(event)
			c.say(cllog.TrustNotSelected(dir, trustSelectWait))
			return
		case <-w.stop:
			w.stopSelecting(event)
			return
		}
	}

selected:
	time.Sleep(typePause)
	if err := c.write("\r"); err != nil {
		c.say(cllog.TrustWriteFailed(dir, err))
		return
	}
	if win.key == "add-dir" {
		c.say(cllog.TrustAddedDir(dir))
	} else {
		c.say(cllog.TrustConfirmed(dir))
	}
}
