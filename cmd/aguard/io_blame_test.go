package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

func TestIOWaitingAttributionAndSummaryLabel(t *testing.T) {
	c := iowatch.Case{Waiting: true, Culprits: &iowatch.Culprit{Main: "/system.slice/adream.service"}}
	if got, want := ioCaseAttribution(&c), "ожидание cgroup adream.service, не фриз машины"; got != want {
		t.Fatalf("подпись ожидания %q, want %q", got, want)
	}
	var out bytes.Buffer
	writeIOReport(&out, &iowatch.Summary{Waiting: 1, WaitingIdleMS: 10_000, WaitingWho: []iowatch.RankedCases{{Name: "adream.service", Cases: 1}}})
	if !strings.Contains(out.String(), "ожидание cgroup при малой занятости диска (не фриз машины): 1") {
		t.Fatalf("сводка ожиданий пропала: %s", out.String())
	}
}

func TestIOStatWriterIsNotReportedAsReaderOrPagingVictim(t *testing.T) {
	c := iowatch.Case{SwapIn: 20_000, Culprits: &iowatch.Culprit{Main: "/system.slice/docker.service", MainRead: 50 << 20, MainWrite: 120 << 20}}
	if c.Culprits.ReadMostly() || c.PagingVictim() {
		t.Fatalf("писатель io.stat помечен читающей жертвой: mostly=%v victim=%v", c.Culprits.ReadMostly(), c.PagingVictim())
	}
	if got := ioCaseAttribution(&c); strings.Contains(got, "в основном чтение") {
		t.Fatalf("подпись писателя io.stat ошибочно говорит о чтении: %q", got)
	}
	o := &ioOwnerFinder{units: map[string]string{"docker.service": "docker-master"}}
	route, err := o.routeCase(&c)
	if err != nil || route.Owner != "docker-master" || route.Victim {
		t.Fatalf("маршрут писателя io.stat: %+v err=%v", route, err)
	}
}

func TestIOLoadCasesReattributesFile(t *testing.T) {
	a, _ := testIOApp(t)
	path := a.cfgPath + ".jsonl"
	now := time.Now()
	c := iowatch.Case{Start: now.Add(-time.Minute), End: now, DurMS: 4000, BusyMS: 50,
		Culprits: &iowatch.Culprit{Main: "/user.slice/agent-lead.scope", MainValue: 1 << 20, IdleWindowMS: 15_000,
			CgroupIdle: []iowatch.Ranked{{Name: "adream.service", Value: 12_000}}}}
	if err := iowatch.SaveCases(path, []iowatch.Case{c}); err != nil {
		t.Fatal(err)
	}
	cfg := iowatch.Default()
	got, err := a.ioLoadCases(&cfg, path, false, false, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.cases) != 1 || !got.cases[0].Waiting || got.cases[0].Culprits.Main != "adream.service" {
		t.Fatalf("file cases not reattributed: %+v", got.cases)
	}
}

func TestIOLoadCasesReattributesLiveService(t *testing.T) {
	s, _, socket := ioLiveService(t, t.Context())
	now := time.Now()
	s.cases = []iowatch.Case{{Start: now.Add(-time.Minute), End: now, DurMS: 4000, BusyMS: 50,
		Culprits: &iowatch.Culprit{Main: "/user.slice/agent-lead.scope", MainValue: 1 << 20, IdleWindowMS: 15_000,
			CgroupIdle: []iowatch.Ranked{{Name: "adream.service", Value: 12_000}}}}}
	go s.serve()
	go s.wait(time.Minute)
	acceptWaitSocket(t, socket)
	cfg := s.cfg
	cfg.Socket = socket
	got, err := s.a.ioLoadCases(&cfg, s.path, true, false, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.cases) != 1 || !got.cases[0].Waiting || got.cases[0].Culprits.Main != "adream.service" {
		t.Fatalf("live cases not reattributed: %+v", got.cases)
	}
}
