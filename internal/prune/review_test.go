package prune

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	thirdID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	fourthID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	fifthID  = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
)

func TestPruneLeavesNonUUIDProjectDirectories(t *testing.T) {
	c, now := world(t)
	path := filepath.Join(c.Root, "-project", "not-a-session")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), c, baseHooks(t, now))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, path) {
		t.Fatal("non-UUID project directory was removed")
	}
	foreignProject := filepath.Join(c.Root, "bash-edit-diff")
	foreignSession := filepath.Join(foreignProject, oldID)
	if err := os.MkdirAll(foreignSession, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreignSession, now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r = Run(context.Background(), c, baseHooks(t, now))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, foreignSession) {
		t.Fatal("top-level directory not beginning with '-' was treated as a Claude project")
	}
}

func TestTicketRootsFollowAndDeduplicateSymlinks(t *testing.T) {
	home := t.TempDir()
	fixtures := []struct{ root, id string }{
		{filepath.Join(home, "docs", "tasks"), oldID},
		{filepath.Join(home, "git", "repo", "docs", "tasks"), otherID},
		{filepath.Join(home, "git", "tasks"), thirdID},
		{filepath.Join(home, "work", "tasks"), fourthID},
	}
	for _, fixture := range fixtures {
		writeTicket(t, fixture.root, fixture.id, "открыт")
	}
	target := filepath.Join(home, "task-target")
	writeTicket(t, target, fifthID, "в работе")
	for _, alias := range []string{"alpha", "beta"} {
		link := filepath.Join(home, "work", alias, "docs", "tasks")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := resolvedTicketRoots(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 5 { // docs, git/repo, git/tasks, direct work/tasks, and one canonical alias target
		t.Fatalf("task roots were not canonicalized/deduplicated: %v", roots)
	}
	reads := 0
	ids, err := openTicketIDs(t.Context(), home, func(context.Context) error { reads++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{oldID, otherID, thirdID, fourthID, fifthID} {
		if !ids[id] {
			t.Errorf("UUID from task root %s missing", id)
		}
	}
	if reads != 5 {
		t.Fatalf("overlapping roots reread ticket files: visits=%d want=5", reads)
	}
}

func TestTicketWalkSkipsVanishedFileButFailsOnBadRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "docs", "tasks")
	writeTicket(t, root, oldID, "открыт")
	last := filepath.Join(root, "z.md")
	if err := os.WriteFile(last, []byte("Статус: открыт\n"+otherID), 0o600); err != nil {
		t.Fatal(err)
	}
	visits := 0
	ids, err := openTicketIDs(t.Context(), home, func(context.Context) error {
		visits++
		if visits == 1 {
			return os.Remove(last)
		}
		return nil
	})
	if err != nil || !ids[oldID] || ids[otherID] {
		t.Fatalf("vanished ticket should be skipped and the root scan should continue: ids=%v err=%v", ids, err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openTicketIDs(t.Context(), home, nil); err == nil {
		t.Fatal("invalid task root was silently treated as an absent root")
	}
	c, now := world(t)
	badRoot := filepath.Join(c.Home, "docs", "tasks")
	if err := os.MkdirAll(filepath.Dir(badRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := baseHooks(t, now)
	h.References = func(ctx context.Context, since time.Time, y func(context.Context) error) (*References, error) {
		return NewReferences(ctx, c.Home, c.Projects, since, y)
	}
	r := Run(context.Background(), c, h)
	if r.Err == nil || !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatalf("task root error did not fail closed: report=%+v", r)
	}
}

func TestDanglingSymlinkTicketRootFailsClosed(t *testing.T) {
	home := t.TempDir()
	link := filepath.Join(home, "work", "repo", "docs", "tasks")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "work", "tasks", "repo"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := openTicketIDs(t.Context(), home, nil); err == nil {
		t.Fatal("dangling task-root symlink was treated as an absent root")
	}
}

func TestRunConvertsPanicToPassError(t *testing.T) {
	c, now := world(t)
	h := baseHooks(t, now)
	h.Live = func(context.Context) (map[string]bool, error) { panic("synthetic liveness failure") }
	r := Run(context.Background(), c, h)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "synthetic liveness failure") {
		t.Fatalf("panic was not returned as a pass error: %+v", r)
	}
}

func TestMountBoundaryIsDetectedFromDeviceNumber(t *testing.T) {
	if !crossesMount(10, 11) || crossesMount(10, 10) {
		t.Fatal("device number comparison did not identify the mount boundary")
	}
	item := Candidate{}
	_, err := (&scanPass{}).handleCandidateError(&item, fmt.Errorf("%w: child", ErrMountBoundary), "")
	if err != nil || !strings.Contains(item.Reason, "точка монтирования") {
		t.Fatalf("mount boundary was not reported as a skip: reason=%q err=%v", item.Reason, err)
	}
}

func TestPreflightRejectsDifferentDevice(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	candidate := filepath.Join(parent, "session")
	if err := os.MkdirAll(filepath.Join(candidate, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	mountID, err := mountIDAt(fd, "session", unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(parent, &stat); err != nil {
		t.Fatal(err)
	}
	statEntry := func(fd int, path string, out *unix.Stat_t, flags int) error {
		if err := unix.Fstatat(fd, path, out, flags); err != nil {
			return err
		}
		if path == "nested" {
			out.Dev = stat.Dev + 1
		}
		return nil
	}
	err = preflightDirAtWithStat(fd, "session", stat.Dev, mountID, func(context.Context) error { return nil }, t.Context(), statEntry)
	if !errors.Is(err, ErrMountBoundary) {
		t.Fatalf("preflight different-device entry err=%v", err)
	}
}

func TestRemovalRejectsDifferentDeviceEntry(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	session := filepath.Join(parent, "session")
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(session, "precious.txt")
	if err := os.WriteFile(victim, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(session, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	mountID, err := mountIDAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(session, &stat); err != nil {
		t.Fatal(err)
	}
	statEntry := func(fd int, path string, out *unix.Stat_t, flags int) error {
		if err := unix.Fstatat(fd, path, out, flags); err != nil {
			return err
		}
		out.Dev = stat.Dev + 1
		return nil
	}
	err = removeSessionEntryWithStat(fd, "session", "precious.txt", stat.Dev, mountID, func(context.Context) error { return nil }, t.Context(), statEntry)
	if !errors.Is(err, ErrMountBoundary) {
		t.Fatalf("remove different-device entry err=%v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("different-device entry was removed: %v", err)
	}
}

func TestRecheckAgeProtectsSessionWithNewFile(t *testing.T) {
	c, now := world(t)
	h := baseHooks(t, now)
	measurements := 0
	h.Measure = func(_ context.Context, path string) (int64, time.Time, error) {
		if strings.Contains(path, oldID) {
			measurements++
			changed := now.Add(-10 * 24 * time.Hour)
			if measurements > 1 {
				changed = now.Add(-time.Hour)
			}
			return 5, changed, nil
		}
		return 5, now.Add(-10 * 24 * time.Hour), nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) || measurements < 2 {
		t.Fatalf("fresh content seen during the final age check did not protect the session: %+v", r)
	}
}

func TestLiveCheckIsLastCandidateRevalidation(t *testing.T) {
	c, now := world(t)
	h := baseHooks(t, now)
	var calls []string
	h.Live = func(context.Context) (map[string]bool, error) {
		calls = append(calls, "live")
		if len(calls) > 1 {
			return map[string]bool{oldID: true}, nil
		}
		return map[string]bool{}, nil
	}
	h.Tickets = func(context.Context) (map[string]bool, error) {
		calls = append(calls, "tickets")
		return map[string]bool{}, nil
	}
	h.Measure = func(_ context.Context, path string) (int64, time.Time, error) {
		if strings.Contains(path, oldID) {
			calls = append(calls, "measure")
		}
		return 1, now.Add(-10 * 24 * time.Hour), nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	want := []string{"live", "tickets", "measure", "tickets", "measure", "live"}
	if len(calls) < len(want) {
		t.Fatalf("revalidation calls=%v, want prefix %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("revalidation calls=%v, want prefix %v", calls, want)
		}
	}
	if exists(t, filepath.Join(c.Root, "-project", otherID)) || !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatalf("candidate was not preserved by final liveness check: %+v", r.Items)
	}
}

func TestTicketStatusesProtectOnlyOpenCandidates(t *testing.T) {
	c, now := world(t)
	open := []struct{ id, status string }{
		{oldID, "открыт"}, {otherID, "в работе"}, {thirdID, "ждёт"}, {fourthID, "на проверке"},
	}
	for _, item := range append(open, struct{ id, status string }{fifthID, "закрыт"}) {
		if item.id != oldID && item.id != otherID {
			makeOldSession(t, c.Root, item.id, now)
		}
		writeTicket(t, filepath.Join(c.Home, "docs", "tasks"), item.id, item.status)
	}
	h := baseHooks(t, now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	h.Tickets = func(ctx context.Context) (map[string]bool, error) { return openTicketIDs(ctx, c.Home, nil) }
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, item := range open {
		if !exists(t, filepath.Join(c.Root, "-project", item.id)) {
			t.Errorf("status %q did not protect %s", item.status, item.id)
		}
	}
	if exists(t, filepath.Join(c.Root, "-project", fifthID)) {
		t.Fatal("closed ticket protected its UUID")
	}
}

func TestOldTranscriptDoesNotProtectSession(t *testing.T) {
	c, now := world(t)
	path := filepath.Join(c.Projects, "p", "old.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("claude-tmp/claude-1000/-project/"+oldID+"/scratchpad"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	h := baseHooks(t, now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	h.References = func(ctx context.Context, since time.Time, y func(context.Context) error) (*References, error) {
		return NewReferences(ctx, c.Home, c.Projects, since, y)
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatal("transcript older than seven days protected the session")
	}
}

func TestRealMeasureUnreadableSubdirectorySkipsCandidate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 directories")
	}
	c, now := world(t)
	blocked := filepath.Join(c.Root, "-project", oldID, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if _, _, err := measure(filepath.Join(c.Root, "-project", oldID), nil, t.Context()); err == nil {
		t.Fatal("measure unexpectedly read an unreadable subtree")
	}
	r := Run(context.Background(), c, baseHooks(t, now))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) || !strings.Contains(r.Items[0].Reason, "ошибка обхода") {
		t.Fatalf("real measure error was not reported or session was deleted: %+v", r.Items)
	}
}

func TestTranscriptLineOver16MiBAndUUIDNoise(t *testing.T) {
	file := filepath.Join(t.TempDir(), "long.jsonl")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat("x", 64*1024)
	remaining := (16 << 20) - 10 // scratchpad marker straddles the reader's 64 KiB chunk boundary
	for remaining > 0 {
		n := min(len(block), remaining)
		if _, err := f.WriteString(block[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	path := "claude-tmp/claude-1000/-project/" + oldID + "/scratchpad"
	if _, err := f.WriteString(path + "\nnoise " + otherID + "\nclaude-tmp/claude-1000/-project/" + thirdID + "not-a-directory"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	if err := scanFreshTranscript(t.Context(), file, nil, ids); err != nil {
		t.Fatal(err)
	}
	if !ids[oldID] || ids[otherID] || ids[thirdID] {
		t.Fatalf("streaming transcript parser missed scratchpad or accepted UUID noise: %v", ids)
	}
}

func TestReferencesRefreshTranscriptAppendBeforeDeletion(t *testing.T) {
	c, now := world(t)
	h := baseHooks(t, now)
	h.Live = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	h.References = func(ctx context.Context, since time.Time, y func(context.Context) error) (*References, error) {
		path := filepath.Join(c.Projects, "p", "existing.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte("initial transcript content"), 0o600); err != nil {
			return nil, err
		}
		old := now.Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			return nil, err
		}
		refs, err := NewReferences(ctx, c.Home, c.Projects, since, y)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(" appended claude-tmp/claude-1000/-project/" + oldID + "/scratchpad"); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		newer := now.Add(time.Second)
		if err := os.Chtimes(path, newer, newer); err != nil {
			return nil, err
		}
		return refs, nil
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatalf("scratchpad reference added after initial snapshot did not protect session: %+v", r.Items)
	}
}

func TestReferencesRefreshDropsClosedTicketReference(t *testing.T) {
	c := t.TempDir()
	ticketRoot := filepath.Join(c, "docs", "tasks")
	writeTicket(t, ticketRoot, oldID, "открыт")
	ticket := filepath.Join(ticketRoot, oldID+".md")
	projects := filepath.Join(c, "projects")
	if err := os.Mkdir(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	refs, err := NewReferences(t.Context(), c, projects, now.Add(-7*24*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !refs.TicketIDs()[oldID] {
		t.Fatal("initial open ticket reference missing")
	}
	if err := os.WriteFile(ticket, []byte("Статус: закрыт\n"+oldID), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := now.Add(time.Second)
	if err := os.Chtimes(ticket, newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := refs.Refresh(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if refs.TicketIDs()[oldID] {
		t.Fatal("closed ticket retained stale protection after refresh")
	}
}

func TestFreshOwnTranscriptFilenameProtectsSession(t *testing.T) {
	c, now := world(t)
	path := filepath.Join(c.Projects, "-home-x", oldID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"sessionId":"`+oldID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	h := baseHooks(t, now)
	h.References = func(ctx context.Context, since time.Time, y func(context.Context) error) (*References, error) {
		return NewReferences(ctx, c.Home, c.Projects, since, y)
	}
	r := Run(context.Background(), c, h)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if !exists(t, filepath.Join(c.Root, "-project", oldID)) {
		t.Fatal("fresh UUID-named transcript did not protect its session")
	}
}

func TestScratchpadUUIDAcceptsCommonPathTerminators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	for _, terminator := range []string{`)`, ".", ":", ";", "]", "|", "&", "*", "?", "`", "<", ">", "!", "#"} {
		t.Run(terminator, func(t *testing.T) {
			content := `claude-tmp/claude-1000/-project/` + oldID + terminator + " next\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			if err := scanFreshTranscript(t.Context(), path, nil, ids); err != nil {
				t.Fatal(err)
			}
			if !ids[oldID] {
				t.Fatalf("UUID not found after terminator %q", terminator)
			}
		})
	}
}

func TestReferencesRefreshDetectsOlderMtimeReplacement(t *testing.T) {
	home := t.TempDir()
	ticketRoot := filepath.Join(home, "docs", "tasks")
	writeTicket(t, ticketRoot, otherID, "готово")
	ticket := filepath.Join(ticketRoot, otherID+".md")
	projects := filepath.Join(home, "projects")
	if err := os.Mkdir(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	refs, err := NewReferences(t.Context(), home, projects, now.Add(-7*24*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(home, "prepared.md")
	if err := os.WriteFile(tmp, []byte("Статус: открыт\n"+oldID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	older := now.Add(-time.Hour)
	if err := os.Chtimes(tmp, older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, ticket); err != nil {
		t.Fatal(err)
	}
	if err := refs.Refresh(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !refs.TicketIDs()[oldID] {
		t.Fatal("replacement ticket with older mtime was not re-read")
	}
}

func TestReferencesRefreshDoesNotReadUnchangedTranscripts(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	project := filepath.Join(projects, "-home-x")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, oldID+".jsonl")
	if err := os.WriteFile(path, []byte(`{"sessionId":"`+oldID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	refs, err := NewReferences(t.Context(), home, projects, now.Add(-7*24*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	refs.readTranscript = func(ctx context.Context, path string, yield func(context.Context) error, ids map[string]bool) error {
		reads++
		return scanFreshTranscript(ctx, path, yield, ids)
	}
	if err := refs.Refresh(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("unchanged transcript reads=%d, want 0", reads)
	}
}

func writeTicket(t *testing.T, root, id, status string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s.md", id)
	if err := os.WriteFile(filepath.Join(root, name), []byte("Статус: "+status+"\n"+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeOldSession(t *testing.T, root, id string, now time.Time) {
	t.Helper()
	path := filepath.Join(root, "-project", id)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "payload")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
