package queue

import (
	"strings"
	"testing"
)

// q-тикет закрыт с ответом пользователя в tk done --check: письмо заказчику несёт этот текст, иначе ответ теряется
// (карточка 53, 27.11: до dream-master дошёл только заголовок, ответ «а» исполнили через 16 ч).
func TestBackQuestionCarriesAnswer(t *testing.T) {
	cases := []struct {
		name, slug, status, check string
		want, absent              string
	}{
		{"q готово — ответ в письме", "q-pa-verify", "готово", "«а» (tl 27d21ddd:61023)", "ответ (из --check): «а» (tl 27d21ddd:61023)", ""},
		{"многострочный ответ — одной строкой", "q-pa-verify", "готово", "«а»\nи ещё", "ответ (из --check): «а» и ещё", "\n"},
		{"q отклонён — прежнее письмо, старый check не в счёт", "q-pa-verify", "отклонён", "«а» до переоткрытия", "", "ответ:"},
		{"q готово без check — прежнее письмо", "q-pa-verify", "готово", "", "", "ответ:"},
		{"обычный тикет — прежнее письмо", "x", "готово", "тесты зелёные", "", "ответ:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(ModeOn)
			s := seeded(w)
			w.live["c-master"], w.free["c-master"] = true, true
			tk := tick("MED", "открыт")
			tk.Slug, tk.Back = tc.slug, "c-master"
			step(s, []Ticket{tk}, w)
			tk.Status, tk.Check = tc.status, tc.check
			ns := step(s, []Ticket{tk}, w)
			if !has(ns, KindBack, "c-master") {
				t.Fatalf("письма заказчику нет: %+v", ns)
			}
			line := ns[0].Line
			if !strings.HasPrefix(line, "aguard: закрыт m/"+tc.slug+" ("+tc.status+") — заголовок") {
				t.Errorf("начало письма: %q", line)
			}
			if tc.want != "" && !strings.Contains(line, tc.want) {
				t.Errorf("нет %q в %q", tc.want, line)
			}
			if tc.absent != "" && strings.Contains(line, tc.absent) {
				t.Errorf("лишнее %q в %q", tc.absent, line)
			}
			if tc.want == "" && line != "aguard: закрыт m/"+tc.slug+" ("+tc.status+") — заголовок" {
				t.Errorf("прежнее письмо изменилось: %q", line)
			}
		})
	}
}
