package codexwatch

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Взвод досрочного сброса: при «да» пользователя на окно демон сам вызывает consume при 100% (пользователь 02.11
// 18:35: «Ты можешь его "взвести" в aguard, чтобы он уже выполнил его когда придёт время»,
// /home/aisee/.claude/projects/-home-aisee/2a879c2d-c2fc-4e0f-bca9-f77994efeca0.jsonl:2696). Исполнитель один:
// ключ идемпотентности окна пишется на диск до вызова, повтор — тем же ключом.

// Reset — взвод в окне: ключ, был ли вызов, исход.
type Reset struct {
	Key     string    `json:"key,omitempty"`     // idempotencyKey окна; создаётся до первого вызова
	Called  bool      `json:"called,omitempty"`  // consume вызывался (ответа могло не быть)
	Outcome string    `json:"outcome,omitempty"` // ответ consume; не пусто — в окне больше не звать
	At      time.Time `json:"at,omitzero"`       // когда пришёл исход
}

// Delivery — исход взвода к доставке: письмом лиду и строкой в тикет; каждое — до успеха.
type Delivery struct {
	Message string    `json:"message"` // письмо лиду
	Note    string    `json:"note"`    // строка в тикет: без дат — tk отвергает время позже записи
	At      time.Time `json:"at"`
	Lead    bool      `json:"lead,omitempty"`   // письмо ушло
	Ticket  bool      `json:"ticket,omitempty"` // строка в тикет записана
}

// SameWindow — одно ли окно: по минуте UTC. В файле одобрений время с точностью до минуты (15:39:00), app-server
// даёт секунды (15:39:23); дрожание секунд не должно заводить окно и ключ заново.
func SameWindow(a, b time.Time) bool {
	return a.UTC().Truncate(time.Minute).Equal(b.UTC().Truncate(time.Minute))
}

// Calling отмечает вызов consume до него: ответ может не дойти, а сброс — пройти.
func (s *State) Calling() { s.Reset.Called = true }

// Done — исход consume: окно сделано, исход — к доставке.
func (s *State) Done(outcome string, r Reading) {
	s.Reset.Outcome, s.Reset.At = outcome, r.At
	s.Pending = append(s.Pending, Delivery{Message: outcomeText(outcome, r, s.Reset.Key), At: r.At,
		Note: fmt.Sprintf("aguard: consume при %.0f%% — outcome=%s (%s); idempotencyKey %s; «да»: %s", r.Used, outcome,
			outcomeWhat[outcome], s.Reset.Key, r.Armed)})
}

// Delivered убирает исходы, доставленные и лиду, и в тикет.
func (s *State) Delivered() {
	keep := s.Pending[:0]
	for _, d := range s.Pending {
		if !d.Lead || !d.Ticket {
			keep = append(keep, d)
		}
	}
	s.Pending = keep
}

// newKey — UUID v4: его советует схема app-server для idempotencyKey.
func newKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read не возвращает ошибку (Go 1.24+)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// outcomeWhat — исходы consume по схеме app-server (codex-cli 0.164.0); другой исход akit/codexlimit возвращает ошибкой.
var outcomeWhat = map[string]string{
	"reset":           "окно сброшено",
	"nothingToReset":  "сбрасывать нечего — сервер не считает окно подходящим",
	"noCredit":        "кредитов сброса нет",
	"alreadyRedeemed": "этот ключ уже сбросил окно раньше",
}

func outcomeText(outcome string, r Reading, key string) string {
	return fmt.Sprintf("aguard codex: досрочный сброс Codex вызван при %.0f%% (взвод, «да»: %s): outcome=%s — %s; плановый сброс "+
		"был %s; idempotencyKey %s.", r.Used, r.Armed, outcome, outcomeWhat[outcome], local(r.ResetsAt), key)
}

func lostText(old, cur time.Time, key string) string {
	return fmt.Sprintf("aguard codex: досрочный сброс Codex вызывался (idempotencyKey %s), ответа не было, а окно сменилось: "+
		"плановый сброс был %s, теперь %s — сброс, видимо, прошёл.", key, local(old), local(cur))
}
