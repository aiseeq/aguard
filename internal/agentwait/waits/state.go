package waits

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

// state is the compact, process independent representation of a Recognizer.
// Pending objects are indexed once so maps retain their shared references.
type state struct {
	Pending    []pendingState    `json:"pending,omitempty"`
	Cur        int               `json:"cur,omitempty"`
	ByCall     map[string]int    `json:"by_call,omitempty"`
	BG         map[string]int    `json:"bg,omitempty"`
	Sends      []int             `json:"sends,omitempty"`
	Wakeup     int               `json:"wakeup,omitempty"`
	TokensTo   int               `json:"tokens_to,omitempty"`
	Tasks      map[string]string `json:"tasks,omitempty"`
	Polls      map[string]int    `json:"polls,omitempty"`
	ShellText  map[string]string `json:"shell_text,omitempty"`
	SessionCmd map[string]string `json:"session_cmd,omitempty"`
	PrevCmd    string            `json:"prev_cmd,omitempty"`
}

type pendingState struct {
	Wait    Wait            `json:"wait"`
	Limit   int64           `json:"limit,omitempty"`
	CallAt  time.Time       `json:"call_at"`
	Replies map[string]bool `json:"replies,omitempty"`
	To      string          `json:"to,omitempty"`
	Session string          `json:"session,omitempty"`
	Codex   bool            `json:"codex,omitempty"`
	OutSeen bool            `json:"out_seen,omitempty"`
}

func (r *Recognizer) MarshalJSON() ([]byte, error) {
	ps := r.all()
	slices.SortFunc(ps, func(a, b *pending) int {
		return cmp.Or(a.w.Line-b.w.Line, strings.Compare(a.w.CallIDs[0], b.w.CallIDs[0]))
	})
	ids := make(map[*pending]int, len(ps))
	st := state{Pending: make([]pendingState, len(ps)), Cur: -1, Wakeup: -1, TokensTo: -1,
		ByCall: make(map[string]int, len(r.byCall)), BG: make(map[string]int, len(r.bg)),
		Tasks: maps.Clone(r.tasks), Polls: make(map[string]int, len(r.polls)),
		ShellText: maps.Clone(r.shellText), SessionCmd: maps.Clone(r.sessionCmd), PrevCmd: r.prevCmd}
	for i, p := range ps {
		ids[p] = i
		st.Pending[i] = pendingState{Wait: p.w, Limit: int64(p.limit), CallAt: p.callAt, Replies: p.replies,
			To: p.to, Session: p.session, Codex: p.codex, OutSeen: p.outSeen}
	}
	index := func(p *pending) int {
		if p == nil {
			return -1
		}
		return ids[p]
	}
	st.Cur, st.Wakeup, st.TokensTo = index(r.cur), index(r.wakeup), index(r.tokensTo)
	for k, p := range r.byCall {
		st.ByCall[k] = index(p)
	}
	for k, p := range r.bg {
		st.BG[k] = index(p)
	}
	for _, p := range r.sends {
		st.Sends = append(st.Sends, index(p))
	}
	for k, p := range r.polls {
		st.Polls[k] = index(p)
	}
	return json.Marshal(st)
}

func (r *Recognizer) UnmarshalJSON(b []byte) error {
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	ps := make([]*pending, len(st.Pending))
	for i := range st.Pending {
		p := &st.Pending[i]
		ps[i] = &pending{w: p.Wait, limit: time.Duration(p.Limit), callAt: p.CallAt, replies: p.Replies,
			to: p.To, session: p.Session, codex: p.Codex, outSeen: p.OutSeen}
	}
	get := func(i int) *pending {
		if i < 0 {
			return nil
		}
		if i >= len(ps) {
			return nil
		}
		return ps[i]
	}
	*r = *New()
	r.cur, r.wakeup, r.tokensTo = get(st.Cur), get(st.Wakeup), get(st.TokensTo)
	for k, i := range st.ByCall {
		if p := get(i); p != nil {
			r.byCall[k] = p
		}
	}
	for k, i := range st.BG {
		if p := get(i); p != nil {
			r.bg[k] = p
		}
	}
	for _, i := range st.Sends {
		if p := get(i); p != nil {
			r.sends = append(r.sends, p)
		}
	}
	for k, i := range st.Polls {
		if p := get(i); p != nil {
			r.polls[k] = p
		}
	}
	if st.Tasks != nil {
		r.tasks = st.Tasks
	}
	if st.ShellText != nil {
		r.shellText = st.ShellText
	}
	if st.SessionCmd != nil {
		r.sessionCmd = st.SessionCmd
	}
	r.prevCmd = st.PrevCmd
	return nil
}

// OpenState describes an active wait for the caller's durable offset metadata.
type OpenState struct {
	Line  int
	Start time.Time
}

// OpenStates lists active waits in transcript order.
func (r *Recognizer) OpenStates() []OpenState {
	ps := r.all()
	out := make([]OpenState, 0, len(ps))
	for _, p := range ps {
		out = append(out, OpenState{Line: p.w.Line, Start: p.w.Start})
	}
	slices.SortFunc(out, func(a, b OpenState) int { return cmp.Compare(a.Line, b.Line) })
	return out
}
