package disk

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// Line — строка сигнала: что хозяин получил бы. home сворачивается в ~.
func (r *Record) Line(home string) string {
	path := r.Path
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		path = "~/" + rest
	}
	at := r.At.Local().Format("02.01 15:04")
	var what string
	switch r.Kind {
	case KindGrew:
		what = fmt.Sprintf("%s выросла на %s с %s, теперь %s", path, policy.HumanBytes(r.Delta),
			r.Since.Local().Format("02.01 15:04"), policy.HumanBytes(r.Bytes))
	case KindStale:
		what = fmt.Sprintf("%s (%s) не менялась с %s — остаток временного?", path, policy.HumanBytes(r.Bytes),
			r.Since.Local().Format("02.01 15:04"))
	case KindFast:
		if path == "" {
			what = fmt.Sprintf("свободное место убыло на %s с %s, ни одна запись так не выросла", policy.HumanBytes(r.Delta),
				r.Since.Local().Format("15:04"))
		} else {
			what = fmt.Sprintf("%s выросла на %s с %s — место тает быстро", path, policy.HumanBytes(r.Delta),
				r.Since.Local().Format("15:04"))
		}
	case KindTrace, KindTraceGone:
		what = traceLine(r, path)
	case KindFinding:
		what = r.Text
	default:
		what = r.Kind + " " + path
	}
	to := r.Owner + " (" + r.Why + ")"
	if r.Admin != "" && r.Admin != r.Owner {
		to += " через " + r.Admin
	}
	return fmt.Sprintf("%s %-5s %s → %s; свободно %s", at, r.Kind, what, to, policy.HumanBytes(r.Free))
}

// traceLine — след: род, путь, размер, кто оставил, срок уборки.
func traceLine(r *Record, path string) string {
	t := r.Trace
	if t == nil {
		return path
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s, %s", t.Kind, path, policy.HumanBytes(t.Bytes))
	if t.Copies > 0 {
		fmt.Fprintf(&b, " (копии транскриптов %s)", policy.HumanBytes(t.Copies))
	}
	if t.Producer != "" {
		fmt.Fprintf(&b, ", оставил %s", t.Producer)
	}
	if t.Command != "" {
		fmt.Fprintf(&b, " (%s)", t.Command)
	}
	if !t.Born.IsZero() {
		fmt.Fprintf(&b, ", с %s", t.Born.Local().Format("02.01 15:04"))
		if r.Kind == KindTraceGone {
			fmt.Fprintf(&b, ", прожил %s", r.At.Sub(t.Born).Round(time.Minute))
		}
	}
	if t.Note != "" {
		fmt.Fprintf(&b, "; %s", t.Note)
	}
	if t.Outlived {
		fmt.Fprintf(&b, "; старше срока уборки «%s»", t.Cleanup)
	}
	return b.String()
}

// Print — журнал строками; bad — битые строки журнала.
func Print(w io.Writer, recs []Record, bad int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("домашняя папка: %w", err)
	}
	var b strings.Builder
	for i := range recs {
		b.WriteString(recs[i].Line(home))
		b.WriteByte('\n')
	}
	if len(recs) == 0 {
		b.WriteString("сигналов нет\n")
	}
	if bad > 0 {
		fmt.Fprintf(&b, "битых строк журнала: %d\n", bad)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	return nil
}
