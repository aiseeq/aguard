package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/akit/proc"
)

var (
	rootSessionRE        = regexp.MustCompile(`^/user\.slice/user-0\.slice/session-c([0-9]+)\.scope$`)
	storedSessionRE      = regexp.MustCompile(`^session-c([0-9]+)\.scope$`)
	secretOptionRE       = regexp.MustCompile(`(?i)(--(?:password|passphrase|token|access-token|oauth2-bearer|api-key|secret)(?:=|\s+)|(?:password|passphrase|token|access[_-]?token|secret|api[_-]?key)=)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	dockerPasswordRE     = regexp.MustCompile(`(?i)(\bdocker\s+login\b[^;]*?\s-p(?:\s+|=))("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	curlUserPasswordRE   = regexp.MustCompile(`(?i)(\bcurl\b[^;]*?\s(?:-u\s*|--user(?:=|\s+)))("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	authorizationValueRE = regexp.MustCompile(`(?i)(authorization:\s*[a-z]+\s+)(?:"([^"]*)"|'([^']*)'|([^\s;"']+))`)
	apiKeyHeaderRE       = regexp.MustCompile(`(?i)((?:x-)?api-key:\s*)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	mysqlPwdEnvRE        = regexp.MustCompile(`(?i)(\bMYSQL_PWD=)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	// urlPasswordRE — пароль в URL: scheme://user:pass@host; host:port без @ не трогает.
	urlPasswordRE = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@;'"]+:)([^@\s;'"/]+)(@)`)
)

const (
	sudoJournalTimeout       = 2 * time.Second
	sudoReportJournalTimeout = 15 * time.Second
	sudoHistoricalWindow     = 12 * time.Hour
	sudoLiveWindow           = 24 * time.Hour
	sudoNegativeCacheWindow  = 5 * time.Minute
	sudoLogindAssociationGap = 5 * time.Second
	sudoRedactedValue        = "[REDA" + "CTED]"
)

type sudoCacheEntry struct {
	checkedAt time.Time
	info      *iowatch.SudoInfo
	bootID    string
}

type sudoLookupCache struct {
	mu        sync.Mutex
	bySession map[string][]sudoCacheEntry
}

func newSudoLookupCache() *sudoLookupCache {
	return &sudoLookupCache{bySession: make(map[string][]sudoCacheEntry)}
}

func (c *sudoLookupCache) find(number, bootID string, at time.Time) (*iowatch.SudoInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found *sudoCacheEntry
	entries := c.bySession[number]
	for i := range entries {
		entry := &entries[i]
		if bootID != "" && entry.bootID != bootID {
			continue
		}
		if entry.info == nil {
			if at.Sub(entry.checkedAt).Abs() <= sudoNegativeCacheWindow && (found == nil || entry.checkedAt.After(found.checkedAt)) {
				found = entry
			}
			continue
		}
		if entry.checkedAt.After(at) {
			continue
		}
		if bootID == "" && (entry.info.ClosedAt.IsZero() || at.Before(entry.info.OpenedAt) || !at.Before(entry.info.ClosedAt)) {
			continue
		}
		if found == nil || entry.checkedAt.After(found.checkedAt) {
			found = entry
		}
	}
	if found == nil {
		return nil, false
	}
	if found.info == nil {
		return nil, true
	}
	info := *found.info
	return &info, true
}

func (c *sudoLookupCache) add(number string, checkedAt time.Time, bootID string, info *iowatch.SudoInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.bySession[number]
	kept := entries[:0]
	for _, old := range entries {
		if old.info == nil && checkedAt.Sub(old.checkedAt) > sudoNegativeCacheWindow {
			continue
		}
		kept = append(kept, old)
	}
	entries = kept
	for i := range entries {
		old := &entries[i]
		if old.checkedAt.Equal(checkedAt) && old.bootID == bootID {
			c.bySession[number] = entries
			return
		}
	}
	var copied *iowatch.SudoInfo
	if info != nil {
		value := *info
		copied = &value
	}
	c.bySession[number] = append(entries, sudoCacheEntry{checkedAt: checkedAt, bootID: bootID, info: copied})
}

// sudoInfo enriches only root cgroups, preserves loaded attribution and handles main and foreign sessions independently.
func (a *app) sudoInfo(ctx context.Context, c *iowatch.Case, cache *sudoLookupCache, live bool) error {
	return a.sudoInfoWithTimeout(ctx, c, cache, live, sudoJournalTimeout)
}

func (a *app) sudoInfoWithTimeout(ctx context.Context, c *iowatch.Case, cache *sudoLookupCache, live bool, journalTimeout time.Duration) error {
	if c == nil || c.Culprits == nil {
		return nil
	}
	var lookupErrors []error
	for _, target := range []struct {
		path string
		dst  **iowatch.SudoInfo
	}{
		{caseSessionPath(c.Culprits, false), &c.Sudo}, {caseSessionPath(c.Culprits, true), &c.ForeignSudo},
	} {
		if *target.dst != nil {
			continue
		}
		number, fullRootPath := sessionCandidate(target.path)
		if number == "" {
			continue
		}
		info, cached := cache.find(number, c.BootID, c.Start)
		if cached {
			if info != nil {
				*target.dst = info
			}
			continue
		}
		if info == nil {
			var err error
			info, err = a.lookupSudoSession(ctx, number, c.BootID, c.Start, live && fullRootPath, journalTimeout)
			if err != nil {
				lookupErrors = append(lookupErrors, fmt.Errorf("session-c%s: %w", number, err))
				continue
			}
			cache.add(number, c.Start, infoBootID(info, c.BootID), info)
		}
		if info != nil {
			*target.dst = info
		}
	}
	return errors.Join(lookupErrors...)
}

func caseSessionPath(c *iowatch.Culprit, foreign bool) string {
	if c == nil {
		return ""
	}
	if !foreign {
		return c.Main
	}
	if c.Foreign.Name == "" {
		return ""
	}
	if path := c.CgroupPaths[c.Foreign.Name]; path != "" {
		return path
	}
	return c.Foreign.Name
}

func rootSessionCandidatePath(path string) bool {
	return rootSessionRE.MatchString(path) || storedSessionRE.MatchString(path)
}

func hasRootSessionCandidate(c *iowatch.Case) bool {
	return c != nil && c.Culprits != nil && (rootSessionCandidatePath(caseSessionPath(c.Culprits, false)) || rootSessionCandidatePath(caseSessionPath(c.Culprits, true)))
}

func sessionCandidate(path string) (number string, fullRootPath bool) {
	if match := rootSessionRE.FindStringSubmatch(path); match != nil {
		return match[1], true
	}
	if match := storedSessionRE.FindStringSubmatch(path); match != nil {
		return match[1], false
	}
	return "", false
}

func infoBootID(info *iowatch.SudoInfo, fallback string) string {
	if info != nil && info.BootID != "" {
		return info.BootID
	}
	return fallback
}

func (a *app) lookupSudoSession(parent context.Context, number, caseBootID string, at time.Time, live bool, journalTimeout time.Duration) (*iowatch.SudoInfo, error) {
	until := at.Add(sudoHistoricalWindow).Format(time.RFC3339)
	if live {
		if info := a.liveSudoSession(parent, number, caseBootID, at, until, journalTimeout); info != nil {
			return info, nil
		}
	}
	since := at.Add(-sudoHistoricalWindow).Format(time.RFC3339)
	sudoRows, err := a.readSudoJournal(parent, journalTimeout, since, until, "_COMM=sudo")
	if err != nil {
		return nil, err
	}
	if info := sudoForExactSession(a.log, number, caseBootID, at, sudoRows); info != nil {
		return info, nil
	}
	logindRows, err := a.readSudoJournal(parent, journalTimeout, since, until, "_SYSTEMD_UNIT=systemd-logind.service")
	if err != nil {
		return nil, err
	}
	return sudoForLogindSession(a.log, number, caseBootID, at, sudoRows, logindRows), nil
}

func (a *app) liveSudoSession(ctx context.Context, number, caseBootID string, at time.Time, until string, journalTimeout time.Duration) *iowatch.SudoInfo {
	pids, err := a.sessionPIDs(number)
	if err != nil || len(pids) == 0 {
		a.debugSudoError("read live sudo session PIDs", err)
		return nil
	}
	bootID, err := a.systemBootID()
	if err != nil || (caseBootID != "" && caseBootID != bootID) {
		a.debugSudoError("read live sudo session boot ID", err)
		return nil
	}
	rows, err := a.readSudoJournal(ctx, journalTimeout, at.Add(-sudoLiveWindow).Format(time.RFC3339), until, sudoPIDFilters(pids)...)
	if err != nil {
		a.debugSudoError("read live sudo session journal", err)
		return nil
	}
	return liveSudoForPIDs(a.log, number, rows, pids, bootID, at)
}

func sudoPIDFilters(pids []int) []string {
	if len(pids) == 0 {
		return []string{"_COMM=sudo"}
	}
	filters := make([]string, 0, 3*len(pids))
	for i, pid := range pids {
		if i > 0 {
			filters = append(filters, "+")
		}
		filters = append(filters, "_COMM=sudo", "_PID="+strconv.Itoa(pid))
	}
	return filters
}

func (a *app) readSudoJournal(ctx context.Context, timeout time.Duration, since, until string, filters ...string) ([]journalRecord, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if a.sudoJournalSource != nil {
		rows, err := a.sudoJournalSource(callCtx, since, until, filters...)
		if err != nil && callCtx.Err() != nil {
			return nil, journalContextError(callCtx.Err(), timeout)
		}
		return rows, err
	}
	return readJournal(callCtx, timeout, since, until, filters...)
}

func readJournal(parent context.Context, timeout time.Duration, since, until string, filters ...string) ([]journalRecord, error) {
	args := append([]string{"--no-pager", "-o", "json", "--since", since, "--until", until}, filters...)
	b, err := exec.CommandContext(parent, "journalctl", args...).Output()
	if err != nil {
		if parent.Err() != nil {
			return nil, journalContextError(parent.Err(), timeout)
		}
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return parseJournalJSON(string(b))
}

func journalContextError(err error, timeout time.Duration) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("journalctl: прерван: %w", err)
	}
	return fmt.Errorf("journalctl: таймаут %s: %w", timeout, err)
}

func liveSudoForPIDs(logger *slog.Logger, number string, rows []journalRecord, pids []int, bootID string, at time.Time) *iowatch.SudoInfo {
	pidSet := make(map[string]bool, len(pids))
	for _, pid := range pids {
		pidSet[strconv.Itoa(pid)] = true
	}
	var opened *journalRecord
	for i := range rows {
		row := &rows[i]
		if !pidSet[row.PID] || row.BootID != bootID || !strings.Contains(row.Message, "COMMAND=") {
			continue
		}
		rowAt, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "live sudo command", err)
			continue
		}
		if rowAt.After(at) {
			continue
		}
		if opened == nil || journalTimestampBefore(opened.Timestamp, row.Timestamp) {
			opened = row
		}
	}
	if opened == nil {
		return nil
	}
	info, ok := parseSudoMessage(opened.Message)
	if !ok {
		return nil
	}
	info.Session, info.BootID = "c"+number, bootID
	openedAt, err := journalTimestamp(opened.Timestamp)
	if err != nil {
		debugJournalTimestamp(logger, "live sudo command timestamp", err)
		return nil
	}
	info.OpenedAt = openedAt
	info.Owner, info.OwnerLabel = sudoOwnerFromCgroup(opened.CGroup)
	return &info
}

func sudoForExactSession(logger *slog.Logger, number, bootID string, at time.Time, rows []journalRecord) *iowatch.SudoInfo {
	want := "c" + number
	var selected *journalRecord
	for i := range rows {
		row := &rows[i]
		if (bootID != "" && row.BootID != bootID) || !isRootSessionOpen(*row, want) || (row.Session != want && !strings.HasSuffix(row.CGroup, "/session-"+want+".scope")) {
			continue
		}
		opened, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "sudo PAM session", err)
			continue
		}
		if opened.After(at) {
			continue
		}
		if selected == nil || journalTimestampBefore(selected.Timestamp, row.Timestamp) {
			selected = row
		}
	}
	if selected == nil {
		return nil
	}
	info := sudoCommandForPID(logger, rows, selected.PID, selected.BootID, selected.Timestamp)
	if info != nil {
		info.Session, info.BootID = want, selected.BootID
		info.ClosedAt = sudoPAMSessionClose(logger, rows, selected.PID, selected.BootID, selected.Timestamp)
	}
	return info
}

func sudoForLogindSession(logger *slog.Logger, number, bootID string, at time.Time, sudoRows, logindRows []journalRecord) *iowatch.SudoInfo {
	want := "c" + number
	var event *journalRecord
	for i := range logindRows {
		row := &logindRows[i]
		if (bootID != "" && row.BootID != bootID) || !isRootLogindSession(row.Message, want) {
			continue
		}
		t, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "logind session", err)
			continue
		}
		if t.After(at) {
			continue
		}
		if event == nil || journalTimestampBefore(event.Timestamp, row.Timestamp) {
			event = row
		}
	}
	if event == nil {
		return nil
	}
	var opened *journalRecord
	best := sudoLogindAssociationGap + time.Nanosecond
	for i := range sudoRows {
		row := &sudoRows[i]
		if !strings.Contains(row.Message, "session opened for user root") || row.BootID != event.BootID {
			continue
		}
		t, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "sudo PAM session", err)
			continue
		}
		eventAt, err := journalTimestamp(event.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "logind session", err)
			continue
		}
		distance := t.Sub(eventAt).Abs()
		if distance <= sudoLogindAssociationGap && distance < best {
			opened, best = row, distance
		}
	}
	if opened == nil {
		return nil
	}
	info := sudoCommandForPID(logger, sudoRows, opened.PID, opened.BootID, opened.Timestamp)
	if info != nil {
		info.Session, info.BootID = want, opened.BootID
		info.ClosedAt = logindSessionClose(logger, logindRows, want, opened.BootID, event.Timestamp)
	}
	return info
}

func sudoPAMSessionClose(logger *slog.Logger, rows []journalRecord, pid, bootID, openedAt string) time.Time {
	var closed time.Time
	for i := range rows {
		row := &rows[i]
		if row.PID != pid || row.BootID != bootID || !strings.Contains(row.Message, "session closed for user root") {
			continue
		}
		at, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "sudo PAM session close", err)
			continue
		}
		if journalTimestampBefore(row.Timestamp, openedAt) || !closed.IsZero() && !at.Before(closed) {
			continue
		}
		closed = at
	}
	return closed
}

func logindSessionClose(logger *slog.Logger, rows []journalRecord, want, bootID, openedAt string) time.Time {
	var closed time.Time
	for i := range rows {
		row := &rows[i]
		if row.BootID != bootID || !isRemovedLogindSession(row.Message, want) || journalTimestampBefore(row.Timestamp, openedAt) {
			continue
		}
		at, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "logind session close", err)
			continue
		}
		if closed.IsZero() || at.Before(closed) {
			closed = at
		}
	}
	return closed
}

func isRemovedLogindSession(message, want string) bool {
	return strings.Contains(message, "Removed session "+want+".") || strings.Contains(message, "Removed session '"+want+"'")
}

func isRootLogindSession(message, want string) bool {
	prefix := "New session '" + want + "' of user "
	if !strings.HasPrefix(message, prefix) {
		return false
	}
	user := strings.TrimPrefix(message, prefix)
	fields := strings.Fields(user)
	return len(fields) > 0 && strings.Trim(fields[0], "'.") == "root"
}

func isRootSessionOpen(row journalRecord, want string) bool {
	return strings.Contains(row.Message, "session opened for user root") &&
		(row.Session == want || strings.HasSuffix(row.CGroup, "/session-"+want+".scope"))
}

func sudoCommandForPID(logger *slog.Logger, rows []journalRecord, pid, bootID, noLaterThan string) *iowatch.SudoInfo {
	var selected *journalRecord
	for i := range rows {
		row := &rows[i]
		if row.PID != pid || row.BootID != bootID || !strings.Contains(row.Message, "COMMAND=") || journalTimestampBefore(noLaterThan, row.Timestamp) {
			continue
		}
		if selected == nil || journalTimestampBefore(selected.Timestamp, row.Timestamp) {
			selected = row
		}
	}
	if selected == nil {
		return nil
	}
	info, ok := parseSudoMessage(selected.Message)
	if !ok {
		return nil
	}
	info.Session, info.BootID = selected.Session, selected.BootID
	openedAt, err := journalTimestamp(noLaterThan)
	if err != nil {
		debugJournalTimestamp(logger, "sudo command session", err)
		return nil
	}
	info.OpenedAt = openedAt
	info.Owner, info.OwnerLabel = sudoOwnerFromCgroup(selected.CGroup)
	return &info
}

func sudoOwnerFromCgroup(cgroup string) (string, string) {
	if strings.Contains(cgroup, "/agents.slice/") {
		if name, _, ok := proc.SessionScope(cgroup); ok {
			return name, name
		}
	}
	if strings.Contains(cgroup, "app-org.kde.konsole-") || strings.Contains(cgroup, "app-far2l@") {
		return "", "терминал пользователя (konsole/far2l)"
	}
	return "", "запуск root"
}

func parseSudoMessage(message string) (iowatch.SudoInfo, bool) {
	parts := strings.Split(strings.TrimSpace(message), " ; ")
	values := make(map[string]string, 4)
	user := ""
	command := ""
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if i == 0 {
			if before, after, ok := strings.Cut(part, " : "); ok {
				user, part = strings.TrimSpace(before), after
			}
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "COMMAND" {
			command = value
			if i+1 < len(parts) {
				command += " ; " + strings.Join(parts[i+1:], " ; ")
			}
			break
		}
		values[key] = value
	}
	if values["USER"] != "root" || command == "" {
		return iowatch.SudoInfo{}, false
	}
	for _, prefix := range []string{"/usr/bin/", "/usr/sbin/"} {
		command = strings.TrimPrefix(command, prefix)
	}
	return iowatch.SudoInfo{Command: truncateSudo(maskSudoSecrets(command), 512), TTY: values["TTY"], PWD: values["PWD"], User: user}, true
}

func maskSudoSecrets(command string) string {
	command = maskSudoCommandArguments(command)
	return maskSudoOtherSecrets(command)
}

func maskSudoOtherSecrets(command string) string {
	command = authorizationValueRE.ReplaceAllStringFunc(command, func(value string) string {
		parts := authorizationValueRE.FindStringSubmatch(value)
		if parts[2] != "" || strings.Contains(value, `"`) {
			return parts[1] + `"[REDACTED]"`
		}
		if parts[3] != "" || strings.Contains(value, "'") {
			return parts[1] + `'[REDACTED]'`
		}
		return parts[1] + "[REDACTED]"
	})
	command = apiKeyHeaderRE.ReplaceAllStringFunc(command, func(value string) string {
		parts := apiKeyHeaderRE.FindStringSubmatch(value)
		secret := parts[2]
		if q := matchingOuterQuote(secret); q != 0 {
			return parts[1] + secret[:1] + "[REDACTED]" + secret[len(secret)-1:]
		}
		if secret[0] == '\'' || secret[0] == '"' {
			suffix := ""
			if len(secret) > 1 && (secret[len(secret)-1] == '\'' || secret[len(secret)-1] == '"') {
				suffix = secret[len(secret)-1:]
			}
			return parts[1] + secret[:1] + "[REDACTED]" + suffix
		}
		if last := secret[len(secret)-1]; last == '\'' || last == '"' {
			return parts[1] + "[REDACTED]" + string(last)
		}
		return parts[1] + "[REDACTED]"
	})
	command = mysqlPwdEnvRE.ReplaceAllString(command, "$1[REDACTED]")
	command = urlPasswordRE.ReplaceAllString(command, "$1[REDACTED]$3")
	command = dockerPasswordRE.ReplaceAllString(command, "$1[REDACTED]")
	command = curlUserPasswordRE.ReplaceAllString(command, "$1[REDACTED]")
	command = secretOptionRE.ReplaceAllString(command, "$1[REDACTED]")
	return command
}

type sudoCommandToken struct {
	start, end          int
	text                string
	redirection, target bool
	sudoQuoted          bool
}

type sudoCommandSegment struct {
	start, end int
	tokens     []sudoCommandToken
}

type sudoEdit struct {
	start, end int
	text       string
}

func maskSudoCommandArguments(command string) string {
	tokens := parseSudoArgv(command)
	var edits []sudoEdit
	if script, ok := sudoArgvShellScript(command, tokens); ok {
		masked := maskSudoShellSecrets(script.text)
		if masked != script.text {
			edits = append(edits, sudoEdit{start: script.start, end: script.end, text: encodeSudoArgvToken(masked, script.sudoQuoted)})
		}
	} else {
		edits = append(edits, maskSudoArgvCommands(tokens)...)
	}
	return applySudoEdits(command, edits)
}

func maskSudoArgvCommands(tokens []sudoCommandToken) []sudoEdit {
	commandIndex := sudoCommandIndex(tokens)
	if commandIndex < 0 || commandIndex >= len(tokens) {
		return nil
	}
	var edits []sudoEdit
	name := sudoExecutable(tokens[commandIndex].text)
	if isMySQLCommand(name) {
		edits = append(edits, maskSudoArgvMySQLArgs(tokens[commandIndex+1:])...)
	}
	if name == "htpasswd" {
		edits = append(edits, maskSudoArgvHtpasswdArg(tokens, commandIndex)...)
	}
	return edits
}

func maskSudoArgvMySQLArgs(tokens []sudoCommandToken) []sudoEdit {
	var edits []sudoEdit
	for _, token := range tokens {
		if maskMySQLPasswordToken(token.text) != token.text {
			edits = append(edits, maskSudoArgvMySQLToken(token))
		}
	}
	return edits
}

func maskSudoArgvHtpasswdArg(tokens []sudoCommandToken, commandIndex int) []sudoEdit {
	if !hasHtpasswdBatchFlag(tokens[commandIndex+1:]) {
		return nil
	}
	if last := len(tokens) - 1; last > commandIndex {
		token := tokens[last]
		masked := sudoRedactedValue
		if token.sudoQuoted {
			masked = encodeSudoArgvToken(masked, true)
		}
		return []sudoEdit{{start: token.start, end: token.end, text: masked}}
	}
	return nil
}

func maskSudoShellSecrets(command string) string {
	command = maskSudoShellCommandArguments(command)
	return maskSudoOtherSecrets(command)
}

func maskSudoShellCommandArguments(command string) string {
	var edits []sudoEdit
	for _, segment := range parseSudoCommandSegments(command) {
		if script, ok := shellCommandScript(segment.tokens); ok {
			inner := command[script.start:script.end]
			masked := maskSudoShellSecrets(inner)
			if masked != inner {
				edits = append(edits, sudoEdit{start: script.start, end: script.end, text: encodeShellScript(masked, command[script.start-1:script.start])})
			}
			continue
		}
		commandIndex := sudoCommandIndex(segment.tokens)
		if commandIndex < 0 || commandIndex >= len(segment.tokens) {
			continue
		}
		name := sudoExecutable(segment.tokens[commandIndex].text)
		if isMySQLCommand(name) {
			for _, token := range segment.tokens[commandIndex+1:] {
				if token.redirection || token.target {
					continue
				}
				if masked := maskMySQLPasswordToken(token.text); masked != token.text {
					edits = append(edits, sudoEdit{start: token.start, end: token.end, text: masked})
				}
			}
		}
		if name == "htpasswd" && hasHtpasswdBatchFlag(segment.tokens[commandIndex+1:]) {
			for i := len(segment.tokens) - 1; i > commandIndex; i-- {
				token := segment.tokens[i]
				if token.redirection || token.target {
					continue
				}
				masked := sudoRedactedValue
				if q := matchingOuterQuote(token.text); q != 0 {
					masked = string(q) + masked + string(q)
				}
				edits = append(edits, sudoEdit{start: token.start, end: token.end, text: masked})
				break
			}
		}
	}
	return applySudoEdits(command, edits)
}

func applySudoEdits(command string, edits []sudoEdit) string {
	if len(edits) == 0 {
		return command
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out strings.Builder
	out.Grow(len(command))
	position := 0
	for _, edit := range edits {
		if edit.start < position || edit.start > len(command) || edit.end < edit.start || edit.end > len(command) {
			continue
		}
		out.WriteString(command[position:edit.start])
		out.WriteString(edit.text)
		position = edit.end
	}
	out.WriteString(command[position:])
	return out.String()
}

func parseSudoArgv(command string) []sudoCommandToken {
	var tokens []sudoCommandToken
	tokenStart := -1
	quoted := false
	inQuote := false
	var value strings.Builder
	finishToken := func(end int) {
		if tokenStart < 0 {
			return
		}
		tokens = append(tokens, sudoCommandToken{start: tokenStart, end: end, text: value.String(), sudoQuoted: quoted})
		tokenStart = -1
		quoted, inQuote = false, false
		value.Reset()
	}
	for i := 0; i < len(command); {
		ch := command[i]
		if tokenStart < 0 && isSudoSpace(ch) {
			i++
			continue
		}
		if tokenStart < 0 {
			tokenStart = i
		}
		if ch == '\\' && i+1 < len(command) && (command[i+1] == '\\' || command[i+1] == '\'') {
			value.WriteByte(command[i+1])
			i += 2
			continue
		}
		if inQuote {
			if ch == '\'' {
				inQuote = false
				i++
				continue
			}
			value.WriteByte(ch)
			i++
			continue
		}
		if isSudoSpace(ch) {
			finishToken(i)
			i++
			continue
		}
		if ch == '\'' {
			quoted, inQuote = true, true
			i++
			continue
		}
		value.WriteByte(ch)
		i++
	}
	finishToken(len(command))
	return tokens
}

func encodeSudoArgvToken(value string, quoted bool) string {
	if !quoted {
		return value
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return "'" + value + "'"
}

func encodeShellScript(value, openingQuote string) string {
	if openingQuote == "'" {
		return strings.ReplaceAll(value, "'", `\'`)
	}
	return value
}

func sudoArgvShellScript(command string, tokens []sudoCommandToken) (sudoCommandToken, bool) {
	commandIndex := sudoCommandIndex(tokens)
	if commandIndex < 0 || commandIndex >= len(tokens) {
		return sudoCommandToken{}, false
	}
	switch strings.ToLower(sudoExecutable(tokens[commandIndex].text)) {
	case "sh", "bash", "dash", "zsh", "ksh":
	default:
		return sudoCommandToken{}, false
	}
	for i := commandIndex + 1; i < len(tokens); i++ {
		option := tokens[i].text
		if strings.HasPrefix(option, "--") {
			if option == "--rcfile" || option == "--init-file" {
				i++
			}
			continue
		}
		if len(option) < 2 || option[0] != '-' || !strings.Contains(option[1:], "c") {
			continue
		}
		if i+1 < len(tokens) {
			script := tokens[i+1]
			if script.text == "--" && i+2 < len(tokens) {
				script = tokens[i+2]
			}
			if !script.sudoQuoted && strings.HasPrefix(script.text, `"`) {
				if quoted, ok := sudoDoubleQuotedScript(command, script); ok {
					return quoted, true
				}
			}
			return script, true
		}
		return sudoCommandToken{}, false
	}
	return sudoCommandToken{}, false
}

func sudoDoubleQuotedScript(command string, token sudoCommandToken) (sudoCommandToken, bool) {
	end := token.start + 1
	escaped := false
	for end < len(command) {
		ch := command[end]
		if escaped {
			escaped = false
		} else if ch == '\\' {
			escaped = true
		} else if ch == '"' {
			return sudoCommandToken{start: token.start + 1, end: end, text: command[token.start+1 : end]}, true
		}
		end++
	}
	return sudoCommandToken{}, false
}

func parseSudoCommandSegments(command string) []sudoCommandSegment {
	var segments []sudoCommandSegment
	segmentStart := 0
	var tokens []sudoCommandToken
	quote := byte(0)
	escaped := false
	redirectTarget := false
	tokenStart := -1
	finishToken := func(end int) {
		if tokenStart < 0 {
			return
		}
		tokens = append(tokens, sudoCommandToken{start: tokenStart, end: end, text: command[tokenStart:end], target: redirectTarget})
		tokenStart = -1
		redirectTarget = false
	}
	finishSegment := func(end int) {
		finishToken(end)
		segments = append(segments, sudoCommandSegment{start: segmentStart, end: end, tokens: tokens})
		segmentStart, tokens = end, nil
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if escaped {
			escaped = false
			continue
		}
		if quote == '\'' {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\\' {
			escaped = true
			if tokenStart < 0 {
				tokenStart = i
			}
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			if tokenStart < 0 {
				tokenStart = i
			}
			continue
		}
		if isSudoSpace(ch) {
			finishToken(i)
			continue
		}
		if ch == '<' || ch == '>' {
			redirection, operatorEnd, hasFD := parseSudoRedirection(command, i, tokenStart)
			if hasFD {
				tokenStart = -1
			} else {
				finishToken(i)
			}
			tokens = append(tokens, redirection)
			redirectTarget = true
			i = operatorEnd - 1
			continue
		}
		if isSudoCommentStart(command, i, tokenStart) {
			finishSegment(i)
			segmentStart = nextSudoLineAfterComment(command, i)
			i = segmentStart - 1
			quote, escaped, redirectTarget = 0, false, false
			continue
		}
		if separatorEnd := sudoSeparatorEnd(command, i); separatorEnd > i {
			finishSegment(i)
			i = separatorEnd - 1
			segmentStart = separatorEnd
			continue
		}
		if tokenStart < 0 {
			tokenStart = i
		}
	}
	finishSegment(len(command))
	return segments
}

func isSudoCommentStart(command string, i, tokenStart int) bool {
	return tokenStart < 0 && command[i] == '#' && !strings.HasPrefix(command[i:], "#012")
}

func nextSudoLineAfterComment(command string, i int) int {
	if nextLine := strings.Index(command[i:], "#012"); nextLine >= 0 {
		return i + nextLine + len("#012")
	}
	return len(command)
}

func parseSudoRedirection(command string, operatorStart, tokenStart int) (sudoCommandToken, int, bool) {
	fdStart := operatorStart
	hasFD := tokenStart >= 0 && allASCIIDigits(command[tokenStart:operatorStart])
	if hasFD {
		fdStart = tokenStart
	}
	operatorEnd := operatorStart + 1
	if operatorEnd < len(command) && command[operatorEnd] == command[operatorStart] {
		operatorEnd++
	}
	return sudoCommandToken{
		start:       fdStart,
		end:         operatorEnd,
		text:        command[fdStart:operatorEnd],
		redirection: true,
	}, operatorEnd, hasFD
}

func sudoSeparatorEnd(command string, i int) int {
	ch := command[i]
	if ch == '#' && strings.HasPrefix(command[i:], "#012") {
		return i + len("#012")
	}
	if ch != ';' && ch != '|' && ch != '&' {
		return i
	}
	if i+1 < len(command) && command[i+1] == ch && (ch == '|' || ch == '&') {
		return i + 2
	}
	return i + 1
}

func isSudoSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n'
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func sudoExecutable(raw string) string {
	return filepath.Base(strings.Trim(raw, `"'`))
}

func isMySQLCommand(name string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "mysql") {
		if strings.HasPrefix(name, "mysql-") {
			return true
		}
		for i := len("mysql"); i < len(name); i++ {
			if !isASCIIWord(name[i]) {
				return false
			}
		}
		return true
	}
	return strings.HasPrefix(name, "mariadb")
}

func maskMySQLPasswordToken(token string) string {
	if !strings.HasPrefix(token, "-p") || len(token) == len("-p") {
		return token
	}
	secret := token[len("-p"):]
	if q := matchingOuterQuote(secret); q != 0 {
		secret = string(q) + sudoRedactedValue + string(q)
	} else {
		secret = sudoRedactedValue
	}
	return "-p" + secret
}

func maskSudoArgvMySQLToken(token sudoCommandToken) sudoEdit {
	masked := maskMySQLPasswordToken(token.text)
	return sudoEdit{start: token.start, end: token.end, text: encodeSudoArgvToken(masked, token.sudoQuoted)}
}

func isASCIIWord(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_'
}

func matchingOuterQuote(value string) byte {
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
		return value[0]
	}
	return 0
}

func hasHtpasswdBatchFlag(tokens []sudoCommandToken) bool {
	for _, token := range tokens {
		if !strings.HasPrefix(token.text, "-") || len(token.text) < 2 {
			continue
		}
		cluster := token.text[1:]
		lettersOnly := true
		for _, r := range cluster {
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
				lettersOnly = false
				break
			}
		}
		if lettersOnly && strings.Contains(cluster, "b") {
			return true
		}
	}
	return false
}

func shellCommandScript(tokens []sudoCommandToken) (sudoCommandToken, bool) {
	commandIndex := sudoCommandIndex(tokens)
	if commandIndex < 0 || commandIndex >= len(tokens) {
		return sudoCommandToken{}, false
	}
	switch strings.ToLower(sudoExecutable(tokens[commandIndex].text)) {
	case "sh", "bash", "dash", "zsh", "ksh":
	default:
		return sudoCommandToken{}, false
	}
	for i := commandIndex + 1; i < len(tokens); i++ {
		if tokens[i].text == "--" {
			continue
		}
		if strings.Contains(tokens[i].text, "c") && strings.HasPrefix(tokens[i].text, "-") {
			if i+1 < len(tokens) {
				script := tokens[i+1]
				if q := matchingOuterQuote(script.text); q != 0 {
					script.start++
					script.end--
				}
				return script, true
			}
			return sudoCommandToken{}, false
		}
	}
	return sudoCommandToken{}, false
}

func sudoCommandIndex(tokens []sudoCommandToken) int {
	for i := 0; i < len(tokens); {
		name := strings.ToLower(sudoExecutable(tokens[i].text))
		switch name {
		case "sudo":
			i = sudoWrapperCommandIndex(tokens, i)
		case "env":
			i = envWrapperCommandIndex(tokens, i)
		case "nice":
			i = niceWrapperCommandIndex(tokens, i)
		case "timeout":
			i = timeoutWrapperCommandIndex(tokens, i)
		case "exec":
			i++
		case "nohup":
			i++
		case "ionice":
			i = ioniceWrapperCommandIndex(tokens, i)
		case "xargs":
			i = xargsWrapperCommandIndex(tokens, i)
		case "docker", "podman":
			if i+1 < len(tokens) && tokens[i+1].text == "exec" {
				i = containerExecCommandIndex(tokens, i)
			} else {
				return i
			}
		default:
			return i
		}
		if i < 0 || i >= len(tokens) {
			return -1
		}
	}
	return -1
}

func ioniceWrapperCommandIndex(tokens []sudoCommandToken, i int) int {
	return wrapperCommandIndex(tokens, i, "-c -n", "cn", "pPu")
}

func xargsWrapperCommandIndex(tokens []sudoCommandToken, i int) int {
	return wrapperCommandIndex(tokens, i, "-I -n -P -L -d -E -s -a --max-args --max-procs --max-lines --delimiter --arg-file --replace", "InPLdEsa", "")
}

func containerExecCommandIndex(tokens []sudoCommandToken, i int) int {
	containerIndex := wrapperCommandIndex(tokens, i+1, "-u -e -w --user --env --workdir --env-file --detach-keys", "uew", "")
	if containerIndex < 0 {
		return -1
	}
	if containerIndex < len(tokens) {
		return containerIndex + 1
	}
	return containerIndex
}

func wrapperCommandIndex(tokens []sudoCommandToken, wrapperIndex int, valueOptions, shortValueOptions, shortNoCommandOptions string) int {
	i := wrapperIndex + 1
	for i < len(tokens) && strings.HasPrefix(tokens[i].text, "-") {
		option := tokens[i].text
		if option == "--" {
			return i + 1
		}
		if strings.HasPrefix(option, "--") {
			if strings.Contains(option, "=") {
				i++
				continue
			}
			if hasWrapperOption(valueOptions, option) {
				i += 2
			} else {
				i++
			}
			continue
		}
		if len(option) == 2 {
			if strings.ContainsRune(shortNoCommandOptions, rune(option[1])) {
				return -1
			}
			if strings.ContainsRune(shortValueOptions, rune(option[1])) {
				i += 2
			} else {
				i++
			}
			continue
		}
		consumedValue := false
		for j := 1; j < len(option); j++ {
			letter := rune(option[j])
			if strings.ContainsRune(shortNoCommandOptions, letter) {
				return -1
			}
			if strings.ContainsRune(shortValueOptions, letter) {
				if j == len(option)-1 {
					i += 2
				} else {
					i++ // the remaining characters are an attached value
				}
				consumedValue = true
				break
			}
		}
		if !consumedValue {
			i++
		}
	}
	return i
}

func hasWrapperOption(options, option string) bool {
	for candidate := range strings.FieldsSeq(options) {
		if candidate == option {
			return true
		}
	}
	return false
}

func sudoWrapperCommandIndex(tokens []sudoCommandToken, i int) int {
	i++
	for i < len(tokens) && strings.HasPrefix(tokens[i].text, "-") {
		option := tokens[i].text
		if option == "--" {
			return i + 1
		}
		if sudoOptionTakesValue(option) && !strings.Contains(option, "=") {
			i += 2
		} else {
			i++
		}
	}
	return i
}

func envWrapperCommandIndex(tokens []sudoCommandToken, i int) int {
	i++
	for i < len(tokens) {
		option := tokens[i].text
		if option == "--" {
			return i + 1
		}
		if strings.Contains(option, "=") && !strings.HasPrefix(option, "-") {
			i++
			continue
		}
		if !strings.HasPrefix(option, "-") {
			return i
		}
		if option == "-u" || option == "--unset" || option == "-C" || option == "--chdir" || option == "-S" || option == "--split-string" {
			i += 2
		} else {
			i++
		}
	}
	return i
}

func niceWrapperCommandIndex(tokens []sudoCommandToken, i int) int {
	i++
	if i < len(tokens) && tokens[i].text == "-n" {
		return i + 2
	}
	if i < len(tokens) && strings.HasPrefix(tokens[i].text, "-n") {
		return i + 1
	}
	if i < len(tokens) && looksLikeNiceAdjustment(tokens[i].text) {
		return i + 1
	}
	return i
}

func timeoutWrapperCommandIndex(tokens []sudoCommandToken, i int) int {
	i++
	for i < len(tokens) && strings.HasPrefix(tokens[i].text, "-") {
		option := tokens[i].text
		if option == "--" {
			return i + 1
		}
		if option == "-k" || option == "-s" || option == "--kill-after" || option == "--signal" {
			i += 2
		} else {
			i++
		}
	}
	if i < len(tokens) {
		i++ // duration
	}
	return i
}

func sudoOptionTakesValue(option string) bool {
	switch option {
	case "-u", "-g", "-h", "-p", "-C", "-R", "-T", "-r", "-t", "-D", "--user", "--group", "--host", "--prompt", "--close-from", "--chroot", "--command-timeout", "--role", "--type", "--chdir":
		return true
	default:
		return false
	}
}

func looksLikeNiceAdjustment(value string) bool {
	if value == "" {
		return false
	}
	for i := range value {
		if i == 0 && (value[i] == '+' || value[i] == '-') {
			continue
		}
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func truncateSudo(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes - len("…")
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

func (a *app) sessionPIDs(number string) ([]int, error) {
	path := filepath.Join(a.cgroupRoot(), "user.slice/user-0.slice/session-c"+number+".scope/cgroup.procs")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sudo session cgroup PIDs: %w", err)
	}
	var pids []int
	for field := range strings.FieldsSeq(string(b)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("parse sudo session PID %q: %w", field, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func journalTimestampBefore(a, b string) bool {
	ta, errA := journalTimestamp(a)
	tb, errB := journalTimestamp(b)
	return errA == nil && errB == nil && ta.Before(tb)
}

func (a *app) cgroupRoot() string {
	if a.cgroupDir != "" {
		return a.cgroupDir
	}
	return "/sys/fs/cgroup"
}

func (a *app) procRoot() string {
	if a.procDir != "" {
		return a.procDir
	}
	return "/proc"
}

func (a *app) debugSudoError(action string, err error) {
	if err != nil && a.log != nil {
		a.log.Debug("aguard io sudo lookup", "action", action, "err", err)
	}
}

func debugJournalTimestamp(logger *slog.Logger, action string, err error) {
	if err != nil && logger != nil {
		logger.Debug("aguard io: invalid journal timestamp", "record", action, "err", err)
	}
}
