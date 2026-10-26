package said

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Header — шапка said.tsv (формат согласован с akit-master для prior и chooks-master для question-check 26.10).
const Header = "time\tsession\tname\tproject\tscope\tkind\twords\tquote\tref\tsupersedes"

// Виды записей.
const (
	KindRule     = "rule"
	KindDecision = "decision"
	KindPromise  = "promise"
)

// Entry — строка said.tsv.
type Entry struct {
	Time       time.Time
	Session    string // полный id
	Name       string // имя сессии
	Project    string // cwd сессии, ~ вместо домашней папки
	Scope      string // global или папка проекта (~/git/x)
	Kind       string
	Words      string // слова для поиска через пробел, строчными
	Quote      string // дословный кусок реплики
	Ref        string // <8 знаков id>:<строка jsonl>
	Supersedes string // ref прежней записи, которую эта отменяет или меняет
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", "").Replace(s)
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// Line — запись строкой said.tsv с переводом строки.
func (e *Entry) Line() string {
	f := []string{e.Time.Format(time.RFC3339), e.Session, e.Name, e.Project, e.Scope, e.Kind, e.Words, e.Quote, e.Ref,
		e.Supersedes}
	for i := range f {
		f[i] = escape(f[i])
	}
	return strings.Join(f, "\t") + "\n"
}

// parseEntry — строка said.tsv; другое число полей — ошибка.
func parseEntry(line string) (Entry, error) {
	f := strings.Split(line, "\t")
	if len(f) != strings.Count(Header, "\t")+1 {
		return Entry{}, fmt.Errorf("полей %d", len(f))
	}
	for i := range f {
		f[i] = unescape(f[i])
	}
	t, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return Entry{}, fmt.Errorf("время: %w", err)
	}
	return Entry{Time: t, Session: f[1], Name: f[2], Project: f[3], Scope: f[4], Kind: f[5], Words: f[6], Quote: f[7],
		Ref: f[8], Supersedes: f[9]}, nil
}

// Read — записи said.tsv; нет файла — пусто. Испорченные строки пропускаются и считаются.
func Read(path string) ([]Entry, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("указатель сказанного: %w", err)
	}
	defer f.Close()
	var out []Entry
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		if sc.Text() == Header || sc.Text() == "" {
			continue
		}
		if e, err := parseEntry(sc.Text()); err == nil {
			out = append(out, e)
		} else {
			bad++ // испорченная строка: счёт уходит вызывающему
		}
	}
	if err := sc.Err(); err != nil {
		return out, bad, fmt.Errorf("указатель сказанного %s: %w", path, err)
	}
	return out, bad, nil
}

// Append дописывает записи: один write с O_APPEND под flock, шапка — если файл пуст. Читателям блокировка не нужна:
// строка не бывает видна наполовину.
func Append(path string, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("указатель сказанного: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return errors.Join(fmt.Errorf("указатель сказанного: блокировка: %w", err), f.Close())
	}
	var b strings.Builder
	if info, err := f.Stat(); err != nil {
		return errors.Join(fmt.Errorf("указатель сказанного: %w", err), f.Close())
	} else if info.Size() == 0 {
		b.WriteString(Header + "\n")
	}
	for i := range entries {
		b.WriteString(entries[i].Line())
	}
	_, werr := f.WriteString(b.String())
	if err := errors.Join(werr, f.Close()); err != nil { // close снимает блокировку
		return fmt.Errorf("указатель сказанного %s: %w", path, err)
	}
	return nil
}

// Candidates — прежние записи той же области (или global), у которых больше всего общих слов с текстом: кандидаты
// на supersedes. Отменённые уже (есть более поздняя запись с supersedes на них) не предлагаются.
func Candidates(entries []Entry, scope, text string, limit int) []Entry {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), notWord) {
		if len([]rune(w)) >= 4 {
			words[w] = true
		}
	}
	gone := map[string]bool{}
	for i := range entries {
		if entries[i].Supersedes != "" {
			gone[entries[i].Supersedes] = true
		}
	}
	type scored struct{ i, n int }
	var list []scored
	for i := range entries {
		e := &entries[i]
		if gone[e.Ref] || (e.Scope != scope && e.Scope != "global" && scope != "global") {
			continue
		}
		n := 0
		for _, w := range strings.FieldsFunc(strings.ToLower(e.Words+" "+e.Quote), notWord) {
			if words[w] {
				n++
			}
		}
		if n >= 2 {
			list = append(list, scored{i, n})
		}
	}
	slices.SortStableFunc(list, func(a, b scored) int {
		if a.n != b.n {
			return b.n - a.n
		}
		return entries[b.i].Time.Compare(entries[a.i].Time)
	})
	out := make([]Entry, 0, min(limit, len(list)))
	for _, s := range list[:min(limit, len(list))] {
		out = append(out, entries[s.i])
	}
	return out
}

func notWord(r rune) bool {
	return !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'а' && r <= 'я' || r == 'ё')
}
