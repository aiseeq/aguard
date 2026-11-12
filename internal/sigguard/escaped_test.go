package sigguard

import (
	"encoding/json"
	"strings"
	"testing"
)

// denyLine — строка сторожа, как её печатает jq tojson: unit и comm — JSON-строки.
func denyLine(unit, comm string) string {
	u, _ := json.Marshal(unit)
	c, _ := json.Marshal(comm)
	return `deny kind=signal arg=15 count=1 first=a last=b span_ms=0 unit=` + string(u) + ` root=7 pid=9 comm=` + string(c) + ` ppid=8 pcomm="bash" euid=1000 target=5 target_comm="sleep" target_unit="b.scope" targets=1 killed=0 mode=enforce`
}

// TestParseJSONRoundTrip — любое значение, напечатанное tojson, разбирается обратно, соседние поля не сдвигаются.
func TestParseJSONRoundTrip(t *testing.T) {
	for _, c := range [][2]string{
		{`worker-a\xd0\xbf.service`, `x"y`},
		{`worker-\`, `a\`},
		{`worker-\\`, `\"`},
		{`u`, ``},
		{`u`, `тест й`},
		{`u`, "a\tb\nc"},
		{`u`, "a b  c"},
		{`u`, "\x01\x7f"},
		{`u`, `="=`},
	} {
		d, ok := Parse(denyLine(c[0], c[1]))
		if !ok || d.Unit != c[0] || d.Comm != c[1] || d.PPID != 8 || d.Pcomm != "bash" || d.Target != 5 || d.TargetUnit != "b.scope" {
			t.Errorf("%q/%q -> %v %#v", c[0], c[1], ok, d)
		}
	}
}

func TestParseUnicodeEscape(t *testing.T) {
	comm := `"` + strings.Join([]string{"", "u0442", "u0435", "ud83d", "ude00", "u0022"}, `\`) + `"`
	d, ok := Parse(`deny kind=signal arg=15 count=1 first=a last=b unit="u" root=7 pid=9 comm=` + comm + ` ppid=8 pcomm="bash" target=5`)
	if !ok || d.Comm != "те😀\"" || d.PPID != 8 {
		t.Errorf("%#v", d)
	}
}

// TestParseBrokenQuote — битая кавычка: в последнем поле значение берётся как есть, в середине строка не разобрана.
func TestParseBrokenQuote(t *testing.T) {
	if d, ok := Parse(`deny kind=signal arg=15 count=1 first=a last=b unit="u" root=7 pid=9 comm="abc`); !ok || d.Comm != "abc" {
		t.Errorf("хвост: %v %#v", ok, d)
	}
	for _, msg := range []string{
		`deny kind=signal arg=15 count=1 first=a last=b unit="u root=7 pid=9 comm="abc" ppid=8`,
		`deny kind=signal arg=15 count=1 first=a last=b unit="u\" root=7 pid=9`,
		`deny kind=signal arg=15 count=1 first=a last=b unit="" root=7 pid=9`,
	} {
		if d, ok := Parse(msg); ok {
			t.Errorf("разобрано: %s -> %#v", msg, d)
		}
	}
}
