package sigguard

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	msg := `deny kind=signal arg=18 count=500 first=14:53:54.032 last=14:53:54.034 span_ms=2 unit="agent-machine-master-11210.scope" root=11219 pid=1002748 comm="python3" ppid=1002744 pcomm="bash" euid=1000 target=1001332 target_comm="sleep" target_unit="asg-probe6.service" mode=enforce`
	d, ok := Parse(msg)
	if !ok || d.Kind != "signal" || d.Count != 500 || d.Unit != "agent-machine-master-11210.scope" || d.TargetComm != "sleep" {
		t.Fatalf("Parse = %#v, %v", d, ok)
	}
	newFormat := `deny kind=signal arg=18 count=10 first=15:25:03.973 last=15:25:03.973 span_ms=0 unit="agent-machine-master-11210.scope" root=11219 pid=2267720 comm="python3" ppid=2267719 pcomm="bash" euid=1000 target=2266005 target_comm="sh" target_unit="asg-mass.service" targets=10 killed=1 mode=enforce`
	d, ok = Parse(newFormat)
	if !ok || d.Targets != 10 || !d.Killed || !d.HasTargets {
		t.Fatalf("new fields: %#v, %v", d, ok)
	}
	d, ok = Parse(msg)
	if !ok || d.Targets != 0 || d.Killed || d.HasTargets {
		t.Fatalf("legacy fields: %#v, %v", d, ok)
	}
	optional := `deny kind=signal arg=15 count=1 first=x last=x unit=worker-job.service root=1 pid=2 comm="Web Content" mode=enforce`
	d, ok = Parse(optional)
	if !ok || d.Comm != "Web Content" || d.PPID != 0 || d.Pcomm != "" || d.Target != 0 || d.TargetComm != "" || d.TargetUnit != "" {
		t.Fatalf("optional fields / quoted spaces: %#v, %v", d, ok)
	}
	d, ok = Parse(`would-deny kind=signal arg=15 count=1 first=x last=x unit=run-abc.service root=1 pid=2 comm="sh" ppid=1 pcomm="systemd" target=3 target_comm="sleep" target_unit="x.service" mode=log`)
	if !ok || d.Mode != "log" {
		t.Fatalf("would-deny: %#v %v", d, ok)
	}
	for _, s := range []string{"bpf_trace_printk: deny x", "systemd[1]: started", "deny nonsense"} {
		if _, ok := Parse(s); ok {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{
		`deny kind=signal count=1 unit=x root=1 pid=1`,
		`deny kind=signal arg=15 unit=x root=1 pid=1`,
		`deny kind=signal arg=15 count=1 unit=x root=1 pid=1 targets=-1`,
		`deny kind=signal arg=15 count=1 unit=x root=1 pid=1 killed=2`,
	} {
		if _, ok := Parse(s); ok {
			t.Errorf("accepted invalid denial %q", s)
		}
	}
}

func TestBatcher(t *testing.T) {
	now := time.Now()
	b := NewBatcher(5 * time.Second)
	d, _ := Parse(`deny kind=signal arg=9 count=1 first=x last=x unit=agent-x-1.scope root=1 pid=2 comm=x ppid=1 pcomm=y target=3 target_comm=z target_unit=z.service`)
	for range 1000 {
		if got := b.Add(d, now); len(got) != 0 {
			t.Fatal(got)
		}
	}
	got := b.Flush(now.Add(5 * time.Second))
	if len(got) != 1 || got[0].Lines != 1000 || got[0].Count != 1000 {
		t.Fatalf("summary %#v", got)
	}
	b.Add(d, now)
	d.Root = "2"
	if got := b.Add(d, now); len(got) != 0 {
		t.Fatal(got)
	}
	got = b.Flush(now.Add(5 * time.Second))
	if len(got) != 2 {
		t.Fatalf("roots %#v", got)
	}
	if got = b.Add(d, now.Add(6*time.Second)); len(got) != 0 {
		t.Fatal(got)
	}
	got = b.Flush(now.Add(11 * time.Second))
	if len(got) != 1 {
		t.Fatalf("next window %#v", got)
	}
}
