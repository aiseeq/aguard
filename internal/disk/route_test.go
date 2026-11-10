package disk

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

func routeConfig() Config {
	c := Default()
	c.Lead = "lead"
	return c
}

func allAlive(string) bool { return true }

func fastRec(path, owner string, bytes, delta, free int64, since time.Time) Record {
	return Record{Kind: KindFast, Path: path, Owner: owner, Why: "session", Bytes: bytes, Delta: delta, Free: free, Since: since, At: since}
}

// Приёмка (1) тикета и сценарий 2 противника 10.11: запись выросла на 5 ГБ — письмо живому хозяину; через 10 мин ещё
// +100 ГБ, свободно 151 ГБ — второе письмо, несмотря на часовую паузу пары, с нынешним остатком.
func TestFastLetterOwnerAndEarlyRepeat(t *testing.T) {
	c := routeConfig()
	st := &State{}
	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	p := "/home/u/.cache/claude-tmp/claude-1000/-home-u-git-life/8e689919"
	got := st.FastLetters(&c, t0, []Record{fastRec(p, "life-master", 10<<30, 5<<30, 256<<30, t0.Add(-23*time.Minute))}, allAlive)
	if len(got) != 1 || got[0].To != "life-master" || got[0].Offline {
		t.Fatalf("первое письмо: %+v", got)
	}
	t1 := t0.Add(10 * time.Minute)
	got = st.FastLetters(&c, t1, []Record{fastRec(p, "life-master", 110<<30, 100<<30, 151<<30, t0)}, allAlive)
	if len(got) != 1 || got[0].To != "life-master" {
		t.Fatalf("второе письмо после +100 ГБ в паузе не пошло: %+v", got)
	}
	if text := got[0].Text(&c, "/home/u"); !strings.Contains(text, "свободно 151 ГБ") || !strings.Contains(text, "+100 ГБ") {
		t.Fatalf("во втором письме нет нынешнего остатка и роста:\n%s", text)
	}
}

// Пауза пары и её снятие раньше срока — по границам: удвоение роста, ещё fast свободного, fast_every.
func TestFastDueBoundaries(t *testing.T) {
	c := routeConfig()
	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	prev := FastTold{At: t0, Bytes: 10 << 30, Grown: 5 << 30, Drop: 5 << 30, Free: 200 << 30}
	cases := []struct {
		name  string
		at    time.Duration
		bytes int64
		free  int64
		path  string
		want  bool
	}{
		{"рост с письма ровно как в письме — удвоение", 10 * time.Minute, 15 << 30, 199 << 30, "/p", true},
		{"рост на байт меньше удвоения", 10 * time.Minute, 15<<30 - 1, 199 << 30, "/p", false},
		{"свободное убыло ровно на fast", 10 * time.Minute, 11 << 30, 195 << 30, "/p", true},
		{"свободное убыло на байт меньше fast", 10 * time.Minute, 11 << 30, 195<<30 + 1, "/p", false},
		{"пауза прошла ровно", time.Hour, 11 << 30, 199 << 30, "/p", true},
		{"до конца паузы секунда", time.Hour - time.Second, 11 << 30, 199 << 30, "/p", false},
		{"убыль без записи: удвоение не считается", 10 * time.Minute, 0, 199 << 30, "", false},
	}
	for _, tc := range cases {
		r := Record{Kind: KindFast, Path: tc.path, Bytes: tc.bytes, Free: tc.free}
		if got := c.fastDue(prev, true, &r, t0.Add(tc.at)); got != tc.want {
			t.Errorf("%s: %v, ждал %v", tc.name, got, tc.want)
		}
	}
	// убыль эпизода больше fast: ранний повтор — по её удвоению, не по fast
	big := FastTold{At: t0, Free: 200 << 30, Grown: 8 << 30, Drop: 8 << 30}
	for _, tc := range []struct {
		free int64
		want bool
	}{{192 << 30, true}, {192<<30 + 1, false}} {
		if got := c.fastDue(big, true, &Record{Kind: KindFast, Free: tc.free}, t0.Add(5*time.Minute)); got != tc.want {
			t.Errorf("убыль эпизода 8 ГБ, свободно %d: %v, ждал %v", tc.free, got, tc.want)
		}
	}
	if !c.fastDue(FastTold{}, false, &Record{Kind: KindFast, Path: "/p"}, t0) {
		t.Error("письма не было — должно идти")
	}
	// Пауза в FastLetters: та же пара через 10 мин с малым ростом — без письма; отметки старше fast_every снимаются.
	st := &State{}
	r := fastRec("/p", "x-master", 10<<30, 5<<30, 200<<30, t0)
	if len(st.FastLetters(&c, t0, []Record{r}, allAlive)) != 1 {
		t.Fatal("первое письмо")
	}
	r.Bytes, r.Free = 12<<30, 198<<30
	if got := st.FastLetters(&c, t0.Add(10*time.Minute), []Record{r}, allAlive); len(got) != 0 {
		t.Fatalf("пара в паузе получила письмо: %+v", got)
	}
	st.FastLetters(&c, t0.Add(2*time.Hour), nil, allAlive)
	if len(st.Letters) != 0 {
		t.Fatalf("старые отметки не сняты: %v", st.Letters)
	}
}

// Сценарий 3: свободное −6 ГБ за 10 мин без виновной записи — письмо лиду «место убыло, виновника обход не нашёл»,
// без выдуманной записи.
func TestFastLetterNoCulpritToLead(t *testing.T) {
	c := routeConfig()
	st := &State{}
	t0 := time.Date(2026, 11, 10, 10, 10, 0, 0, time.Local)
	r := Record{Kind: KindFast, Owner: "lead", Why: "lead", Delta: 6 << 30, Since: t0.Add(-10 * time.Minute), Free: 174 << 30}
	got := st.FastLetters(&c, t0, []Record{r}, allAlive)
	if len(got) != 1 || got[0].To != "lead" {
		t.Fatalf("письмо лиду: %+v", got)
	}
	text := got[0].Text(&c, "/home/u")
	if !strings.Contains(text, "убыло на 6.0 ГБ с 10:00, виновника обход не нашёл") || strings.Contains(text, "\n- ") ||
		strings.Contains(text, "выросли") || !strings.Contains(text, "свободно 174 ГБ") {
		t.Fatalf("текст без виновника:\n%s", text)
	}
}

// Сценарий 8: 30 fast одного хозяина за проход — одно письмо со списком записей, суммой прироста и свободным местом.
func TestFastLetterOnePerOwnerPerPass(t *testing.T) {
	c := routeConfig()
	st := &State{}
	t0 := time.Date(2026, 11, 10, 11, 0, 0, 0, time.Local)
	var recs []Record
	for i := range 30 {
		recs = append(recs, fastRec(fmt.Sprintf("/home/u/.cache/w/%02d", i), "llmark-master", 6<<30, 5<<30, 160<<30, t0.Add(-10*time.Minute)))
	}
	recs = append(recs, fastRec("/home/u/.cache/other", "sc2go-master", 6<<30, 5<<30, 160<<30, t0.Add(-10*time.Minute)))
	got := st.FastLetters(&c, t0, recs, allAlive)
	if len(got) != 2 || got[0].Owner != "llmark-master" || len(got[0].Recs) != 30 || got[1].Owner != "sc2go-master" {
		t.Fatalf("писем %d: %+v", len(got), got)
	}
	text := got[0].Text(&c, "/home/u")
	if n := strings.Count(text, "\n- ~/.cache/w/"); n != 30 || !strings.Contains(text, "на 150 ГБ (30)") || !strings.Contains(text, "свободно 160 ГБ") {
		t.Fatalf("строк записей %d, текст:\n%s", n, text)
	}
}

// Хозяин не в сети — письмо лиду с его именем; хозяин — лид — лиду без пометки.
func TestFastLetterOfflineOwnerToLead(t *testing.T) {
	c := routeConfig()
	st := &State{}
	t0 := time.Date(2026, 11, 10, 11, 0, 0, 0, time.Local)
	recs := []Record{fastRec("/a", "life-master", 6<<30, 5<<30, 160<<30, t0), fastRec("/b", "lead", 6<<30, 5<<30, 160<<30, t0)}
	got := st.FastLetters(&c, t0, recs, func(n string) bool { return n == "lead" })
	if len(got) != 2 || got[0].To != "lead" || !got[0].Offline || got[1].To != "lead" || got[1].Offline {
		t.Fatalf("письма: %+v", got)
	}
	if text := got[0].Text(&c, "/home/u"); !strings.Contains(text, "Хозяин life-master не в сети — пишу лиду") {
		t.Fatalf("текст:\n%s", text)
	}
	// Лид сам не в сети — письмо всё равно ему, без пометки «хозяин не в сети».
	st = &State{}
	got = st.FastLetters(&c, t0, recs[1:], func(string) bool { return false })
	if len(got) != 1 || got[0].To != "lead" || got[0].Offline {
		t.Fatalf("лиду, когда он не в сети: %+v", got)
	}
}

// Сценарий 4: 20 stale одного хозяина в одном обходе — один тикет, одна заметка со всеми 20 (путь, размер, с какого
// времени); новые находки в сутки после — ждут и уходят одной заметкой по концу суток.
func TestTicketBatchOncePerDay(t *testing.T) {
	c := routeConfig()
	ts, err := LoadTickets(t.TempDir() + "/нет.json")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	var recs []Record
	for i := range 20 {
		recs = append(recs, Record{Kind: KindStale, Path: fmt.Sprintf("/home/u/.cache/s%02d", i), Owner: "dream-master",
			Bytes: 2 << 30, Since: t0.Add(-72 * time.Hour), At: t0})
	}
	recs = append(recs, Record{Kind: KindFast, Path: "/f", Owner: "dream-master"}) // fast — письмом, не тикетом
	if !ts.Add(&c, recs) {
		t.Fatal("находки не добавились")
	}
	due := ts.Due(t0)
	if len(due) != 1 || due[0].Class != TicketStale || due[0].Owner != "dream-master" || len(due[0].Recs) != 20 {
		t.Fatalf("тикетов %d: %+v", len(due), due)
	}
	note := due[0].Note("/home/u")
	for i := range 20 {
		if !strings.Contains(note, fmt.Sprintf("~/.cache/s%02d (2.0 ГБ) не менялась с 07.11 10:00", i)) {
			t.Fatalf("в заметке нет s%02d:\n%s", i, note)
		}
	}
	if text := due[0].Text("/home/u", "", ""); strings.Count(text, "\n- ~/.cache/s") != 20 {
		t.Fatalf("в тексте нового тикета не все 20:\n%s", text)
	}
	ts.Done(&due[0], t0, true)
	ts.Add(&c, []Record{{Kind: KindGrew, Path: "/g", Owner: "dream-master", Bytes: 9 << 30, Delta: 6 << 30},
		{Kind: KindStale, Path: "/home/u/.cache/s99", Owner: "dream-master", Bytes: 2 << 30}})
	if got := ts.Due(t0.Add(time.Hour)); len(got) != 1 || got[0].Class != TicketGrew {
		t.Fatalf("другой класс того же хозяина ждать не должен: %+v", got)
	}
	if got := ts.Due(t0.Add(24*time.Hour - time.Second)); len(got) != 1 || got[0].Class != TicketGrew {
		t.Fatalf("stale раньше суток: %+v", got)
	}
	if got := ts.Due(t0.Add(24 * time.Hour)); len(got) != 2 || got[1].Class != TicketStale || len(got[1].Recs) != 1 {
		t.Fatalf("через сутки stale должен уйти: %+v", got)
	}
}

// tk упал — находки ждут, повтор через час; класс тикета следа — от trace_min.
func TestTicketRetryAndTraceMin(t *testing.T) {
	c := routeConfig()
	ts, err := LoadTickets(t.TempDir() + "/нет.json")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	ts.Add(&c, []Record{{Kind: KindTrace, Path: "/small", Owner: "x", Bytes: 1<<30 - 1}, {Kind: KindTrace, Path: "/big", Owner: "", Bytes: 1 << 30}})
	due := ts.Due(t0)
	if len(due) != 1 || due[0].Class != TicketTrace || due[0].Owner != "lead" || due[0].Recs[0].Path != "/big" {
		t.Fatalf("след меньше trace_min в тикет не идёт, без хозяина — лиду: %+v", due)
	}
	ts.Done(&due[0], t0, false)
	if got := ts.Due(t0.Add(time.Hour - time.Second)); len(got) != 0 {
		t.Fatalf("повтор раньше часа: %+v", got)
	}
	if got := ts.Due(t0.Add(time.Hour)); len(got) != 1 {
		t.Fatalf("повтора через час нет: %+v", got)
	}
}

// passLetters — проход Pass и письма по его fast-находкам, как в демоне (diskRound → diskRoute).
func passLetters(t *testing.T, s *State, c *Config, now time.Time, free int64, sizes map[string]int64) ([]Record, []FastLetter) {
	t.Helper()
	list := func() ([]string, error) { return slices.Sorted(maps.Keys(sizes)), nil }
	owner := func(p string) (string, string) {
		if p == "" {
			return c.Lead, "lead"
		}
		return "life-master", "session"
	}
	recs, _, err := s.Pass(c, now, free, list, fakeMeasure(sizes, now), owner)
	if err != nil {
		t.Fatal(err)
	}
	return recs, s.FastLetters(c, now, recs, allAlive)
}

func fastOf(recs []Record) []Record {
	return slices.DeleteFunc(slices.Clone(recs), func(r Record) bool { return r.Kind != KindFast })
}

// Сценарий 2 противника 10.11 сквозь Pass (дефект 1 проверки 10.11 11:40): +5 ГБ в 12:00 — письмо; через 5 мин ещё
// +100 ГБ при свободных 151 ГБ — обход по тревоге (walkGap 2 мин) раньше fast_window, и предел журнала fast не должен
// съесть ни запись, ни второе письмо. Так же убыль без виновника: ещё −6 ГБ через 3 мин после первой — вторая запись и
// письмо лиду.
func TestFastEarlyRepeatThroughPass(t *testing.T) {
	c := testConfig(t, "/home/u")
	c.Lead = "lead"
	s := &State{Sizes: map[string][]Point{}, Told: map[string]time.Time{}}
	p := "/home/u/.cache/claude-tmp/claude-1000/-home-u-git-life/8e689919"
	sizes := map[string]int64{p: 5 * gb}
	t0 := time.Date(2026, 11, 10, 11, 50, 0, 0, time.Local)
	if recs, _ := passLetters(t, s, c, t0, 256*gb, sizes); len(recs) != 0 {
		t.Fatalf("первый обход: %+v", recs)
	}
	sizes[p] = 10 * gb
	recs, letters := passLetters(t, s, c, t0.Add(10*time.Minute), 250*gb, sizes)
	if f := fastOf(recs); len(f) != 1 || f[0].Path != p || len(letters) != 1 || letters[0].To != "life-master" {
		t.Fatalf("первый всплеск: записи %+v, письма %+v", recs, letters)
	}
	sizes[p] = 110 * gb
	recs, letters = passLetters(t, s, c, t0.Add(15*time.Minute), 151*gb, sizes)
	if f := fastOf(recs); len(f) != 1 || f[0].Path != p || f[0].Delta != 100*gb {
		t.Fatalf("второй всплеск через 5 мин не записан: %+v", recs)
	}
	if len(letters) != 1 || letters[0].To != "life-master" || !strings.Contains(letters[0].Text(c, "/home/u"), "свободно 151 ГБ") {
		t.Fatalf("второго письма нет: %+v", letters)
	}
	// убыль без виновника: первая — письмо лиду; ещё −6 ГБ через 3 мин — снова запись и письмо
	recs, letters = passLetters(t, s, c, t0.Add(18*time.Minute), 145*gb, sizes)
	if f := fastOf(recs); len(f) != 1 || f[0].Path != "" || len(letters) != 1 || letters[0].To != "lead" {
		t.Fatalf("убыль без виновника: записи %+v, письма %+v", recs, letters)
	}
	recs, letters = passLetters(t, s, c, t0.Add(21*time.Minute), 139*gb, sizes)
	if f := fastOf(recs); len(f) != 1 || f[0].Path != "" || len(letters) != 1 || letters[0].To != "lead" {
		t.Fatalf("вторая убыль без виновника через 3 мин: записи %+v, письма %+v", recs, letters)
	}
}

// Предел журнала fast остаётся против шума: запись той же пары в fast_window без раннего повтора письма (рост меньше
// удвоения, свободное с письма убыло меньше fast) — ни строки, ни письма.
func TestFastJournalLimitWithoutEarlyRepeat(t *testing.T) {
	c := testConfig(t, "/home/u")
	c.Lead = "lead"
	c.Walk, c.WalkChange = policy.Duration(time.Minute), 1<<30 // плановый обход в окне при тающем месте
	s := &State{Sizes: map[string][]Point{}, Told: map[string]time.Time{}}
	p := "/home/u/.cache/farm-hard"
	sizes := map[string]int64{p: 5 * gb}
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	passLetters(t, s, c, t0, 260*gb, sizes)
	sizes[p] = 10 * gb
	if recs, letters := passLetters(t, s, c, t0.Add(2*time.Minute), 254*gb, sizes); len(fastOf(recs)) != 1 || len(letters) != 1 {
		t.Fatalf("первый всплеск: записи %+v, письма %+v", recs, letters)
	}
	sizes[p] = 13 * gb
	recs, letters := passLetters(t, s, c, t0.Add(4*time.Minute), 254*gb-3*gb/2, sizes)
	if f := fastOf(recs); len(f) != 0 || len(letters) != 0 {
		t.Fatalf("пара в пределе журнала без раннего повтора: записи %+v, письма %+v", f, letters)
	}
	if !s.LastWalk.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("обхода не было: %v — тест не проверяет предел", s.LastWalk)
	}
}

// Дефект 7 проверки 10.11: та же находка о том же пути заменяет прежнюю (новый размер), прочие пути класса остаются —
// в тикете нет дублей и ничего не теряется.
func TestTicketAddReplacesSamePath(t *testing.T) {
	c := routeConfig()
	ts, err := LoadTickets(t.TempDir() + "/нет.json")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	ts.Add(&c, []Record{{Kind: KindStale, Path: "/a", Owner: "x", Bytes: 2 << 30}, {Kind: KindStale, Path: "/b", Owner: "x", Bytes: 3 << 30}})
	ts.Add(&c, []Record{{Kind: KindStale, Path: "/a", Owner: "x", Bytes: 4 << 30}})
	due := ts.Due(t0)
	if len(due) != 1 || len(due[0].Recs) != 2 {
		t.Fatalf("находки: %+v", due)
	}
	byPath := map[string]int64{}
	for _, r := range due[0].Recs {
		byPath[r.Path] = r.Bytes
	}
	if byPath["/a"] != 4<<30 || byPath["/b"] != 3<<30 {
		t.Fatalf("замена прежней находки пути: %v", byPath)
	}
}

// Ровный рост одной записи 2,5 ГБ/мин час подряд (проверка 10.11 12:09: 31 письмо, по одному на обход по тревоге):
// ранний повтор — по удвоению роста эпизода с первого письма, писем — по логарифму роста, не на каждый обход.
func TestFastSteadyGrowthLetterCount(t *testing.T) {
	c := testConfig(t, "/home/u")
	c.Lead = "lead"
	s := &State{Sizes: map[string][]Point{}, Told: map[string]time.Time{}}
	p := "/home/u/.cache/claude-tmp/claude-1000/-home-u-git-life/steady"
	sizes := map[string]int64{p: 5 * gb}
	t0 := time.Date(2026, 11, 10, 13, 0, 0, 0, time.Local)
	free := int64(400 * gb)
	passLetters(t, s, c, t0, free, sizes)
	letters, walks := 0, 0
	for m := 2; m <= 58; m += 2 {
		sizes[p] += 5 * gb
		free -= 5 * gb
		at := t0.Add(time.Duration(m) * time.Minute)
		_, ls := passLetters(t, s, c, at, free, sizes)
		if s.LastWalk.Equal(at) {
			walks++
		}
		letters += len(ls)
	}
	if walks < 25 {
		t.Fatalf("обходов %d из 29 — тест не проверяет обходы по тревоге", walks)
	}
	t.Logf("писем за час ровного роста: %d, обходов %d", letters, walks)
	// эпизод 5 → 10 → 20 → 40 → 80 → 160 ГБ: письма на 2, 4, 8, 16, 32 мин и по свободному так же — не больше 6
	if letters < 2 || letters > 6 {
		t.Fatalf("писем за час ровного роста: %d, ждали 2–6", letters)
	}
}

// Перепроверка 10.11 12:20: удвоение — от первого роста пары (12 ГБ, не порог fast); очистка между письмами (свободное
// выросло) не уводит рост и убыль эпизода в минус — иначе письмо на каждом обходе по тревоге.
func TestFastToldEpisode(t *testing.T) {
	c := routeConfig()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	first := c.told(FastTold{}, false, &Record{Kind: KindFast, Path: "/p", Bytes: 20 << 30, Delta: 12 << 30, Free: 160 << 30}, t0)
	at := t0.Add(5 * time.Minute)
	if c.fastDue(first, true, &Record{Kind: KindFast, Path: "/p", Bytes: 32<<30 - 1, Free: 156 << 30}, at) {
		t.Error("рост на байт меньше первого роста 12 ГБ — письмо раньше удвоения")
	}
	// рост записи на 12 ГБ — письмо, хотя свободное выросло (очистка рядом) до 200 ГБ
	grown := &Record{Kind: KindFast, Path: "/p", Bytes: 32 << 30, Free: 200 << 30}
	if !c.fastDue(first, true, grown, at) {
		t.Fatal("рост ровно на первый рост — удвоение, письмо")
	}
	second := c.told(first, true, grown, at)
	if second.Grown != 24<<30 || second.Drop != first.Drop {
		t.Fatalf("эпизод после очистки: рост %d, убыль %d (ждали 24 ГиБ и прежнюю %d)", second.Grown, second.Drop, first.Drop)
	}
	shrunk := c.told(second, true, &Record{Kind: KindFast, Path: "/p", Bytes: 10 << 30, Free: 150 << 30}, at.Add(time.Minute))
	if shrunk.Grown != second.Grown {
		t.Fatalf("запись уменьшилась — рост эпизода %d, ждали прежний %d", shrunk.Grown, second.Grown)
	}
}
