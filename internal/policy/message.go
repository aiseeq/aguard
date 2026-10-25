package policy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HumanBytes — «1.2 ГБ», «350 МБ», «12 КБ» (двоичные единицы, точка в дробях).
func HumanBytes(b int64) string {
	units := []string{"Б", "КБ", "МБ", "ГБ", "ТБ"}
	f := float64(b)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	switch {
	case i == 0:
		return strconv.FormatInt(b, 10) + " Б"
	case f < 10:
		return strconv.FormatFloat(f, 'f', 1, 64) + " " + units[i]
	}
	return strconv.FormatFloat(f, 'f', 0, 64) + " " + units[i]
}

// HumanAge — «40 с», «25 мин», «3 ч 10 мин», «2 д 4 ч».
func HumanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%d с", s)
	case s < 3600:
		return fmt.Sprintf("%d мин", s/60)
	case s < 86400:
		if m := s % 3600 / 60; m != 0 {
			return fmt.Sprintf("%d ч %d мин", s/3600, m)
		}
		return fmt.Sprintf("%d ч", s/3600)
	}
	if h := s % 86400 / 3600; h != 0 {
		return fmt.Sprintf("%d д %d ч", s/86400, h)
	}
	return fmt.Sprintf("%d д", s/86400)
}

// FormatGBMin — мера «ГБ × минуты» с точкой.
func FormatGBMin(v float64) string {
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// TestMark — первая строка тестовой сводки.
const TestMark = "тест aguard, отвечать не нужно."

func sectionTitle(l *Line) string {
	name := l.CreatorName
	if name == "" {
		name = "без имени"
	}
	switch l.Rel {
	case RelOwn:
		return "Ваши записи:"
	case RelFighter:
		return "Записи ваших бойцов:"
	case RelDeadOwner:
		return "Записи завершившейся сессии " + name + " (вы следующая ступень, уберите за неё):"
	case RelIgnored:
		return fmt.Sprintf("Записи сессии %s, не убранные за %d сводки (напомните ей или уберите сами):", name, l.Sent)
	case RelNonAgent:
		return "Записи не агентов (процессы пользователя вне сессий Claude):"
	case RelOtherUID:
		return "Записи других учёток (root, службы):"
	}
	return "Записи без известного создателя (появились до запуска сторожа):"
}

// Message собирает текст сводки. test — пометка тестовой отправки.
func (c *Config) Message(s *Summary, now time.Time, test bool) string {
	var b strings.Builder
	if test {
		b.WriteString(TestMark + "\n\n")
	}
	b.WriteString("aguard: сводка по временным записям. /tmp и /tmp/claude-tmp — tmpfs, то есть оперативка; " +
		"записи ниже держат её дольше порога. Ненужное удалите, нужное перенесите на диск в scratchpad сессии.\n")
	listed := 0
	section := ""
	for i := range s.Lines {
		l := &s.Lines[i]
		if listed >= c.Policy.MaxListed {
			break
		}
		if t := sectionTitle(l); t != section {
			section = t
			b.WriteString("\n" + t + "\n")
		}
		listed++
		e := &l.Item.Entry
		fmt.Fprintf(&b, "%d. %s — %s", listed, e.Path, HumanBytes(e.Bytes))
		if e.Dir {
			fmt.Fprintf(&b, ", файлов %d", e.Files)
		}
		fmt.Fprintf(&b, ", возраст %s, мера %s ГБ·мин; создал: %s", HumanAge(now.Sub(e.Born)),
			FormatGBMin(l.Item.GBMin()), CreatorText(e.Owner))
		if l.Item.Fighter != "" {
			b.WriteString(", боец " + l.Item.Fighter)
		}
		b.WriteString("\n   действие: " + action(l))
		b.WriteString("\n")
	}
	if rest := len(s.Lines) - listed; rest > 0 {
		fmt.Fprintf(&b, "\nИ ещё записей: %d (меньше по мере).\n", rest)
	}
	budget := FormatGBMin(c.Policy.BudgetGBM)
	if s.Pressure {
		fmt.Fprintf(&b, "\nПорог %s ГБ·мин на владельца сейчас ужесточён до ×%s: память под давлением.",
			budget, strconv.FormatFloat(s.Factor, 'f', -1, 64))
	} else {
		minutes := func(gb float64) string {
			return HumanAge(time.Duration(c.Policy.BudgetGBM / gb * float64(time.Minute)))
		}
		fmt.Fprintf(&b, "\nПорог %s ГБ·мин на владельца (1 ГБ на %s ≈ 100 МБ на %s ≈ 10 МБ на %s).",
			budget, minutes(1), minutes(100.0/1024), minutes(10.0/1024))
	}
	fmt.Fprintf(&b, " Сводка раз в %s; не убранное за %d сводки уходит выше: сессия-создатель, админ пространства, %s.",
		HumanAge(c.Interval.D()), c.Escalation.After, c.Lead)
	return b.String()
}

func action(l *Line) string {
	p := l.Item.Entry.Path
	q := shellQuote(p)
	switch l.Rel {
	case RelOtherUID:
		return "проверить, нужна ли; если нет — sudo rm -rf " + q
	case RelNonAgent, RelNoCreator:
		return "проверить, нужна ли; если нет — rm -rf " + q
	}
	return "rm -rf " + q + " , если не нужна; нужную — mv " + q + " в scratchpad на диске"
}

func shellQuote(s string) string {
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '+' || r == ':' || r == ',' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
