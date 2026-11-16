package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/said"
	tailpkg "github.com/aiseeq/aguard/internal/tail"
)

type saidEvalPair struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
}

type saidEvalGroup struct {
	Ref      string
	Expected []saidEvalPair
}

type saidEvalResult struct {
	Ref       string         `json:"ref"`
	Expected  []saidEvalPair `json:"expected"`
	Actual    []said.Item    `json:"actual,omitempty"`
	ExpectedN int            `json:"expected_count"`
	FoundN    int            `json:"found_count"`
	MatchedN  int            `json:"matched_count"`
	Extra     int            `json:"extra_records"`
	Error     string         `json:"error,omitempty"`
}

type saidEvalSummary struct {
	Cases        []saidEvalResult `json:"cases"`
	ExpectedN    int              `json:"expected_count"`
	FoundN       int              `json:"found_count"`
	MatchedN     int              `json:"matched_count"`
	ExtraRecords int              `json:"extra_records"`
	Errors       int              `json:"errors"`
}

func readSaidEvalCases(path string) ([]saidEvalGroup, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("said eval cases: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	byRef := map[string]int{}
	var groups []saidEvalGroup
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Text()
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		parts := strings.Split(raw, "\t")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, fmt.Errorf("said eval cases %s:%d: ожидались ref, kind, scope через TAB", path, line)
		}
		idx, ok := byRef[parts[0]]
		if !ok {
			idx = len(groups)
			byRef[parts[0]] = idx
			groups = append(groups, saidEvalGroup{Ref: parts[0]})
		}
		groups[idx].Expected = append(groups[idx].Expected, saidEvalPair{Kind: parts[1], Scope: parts[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("said eval cases %s: %w", path, err)
	}
	return groups, nil
}

func (a *app) saidEval(ctx context.Context, c *config.Config, casesPath, promptPath string, asJSON bool) error {
	groups, err := readSaidEvalCases(casesPath)
	if err != nil {
		return err
	}
	prompt, err := readSaidEvalPrompt(promptPath)
	if err != nil {
		return err
	}
	prior, bad, err := said.Read(c.Said.File)
	if err != nil {
		return err
	}
	if bad > 0 {
		a.warn("said", "битые строки указателя", "file", c.Said.File, "count", bad)
	}

	summary := saidEvalSummary{}
	for _, group := range groups {
		row := a.saidEvalGroup(ctx, c, prompt, prior, group)
		summary.Cases = append(summary.Cases, row)
		addSaidEvalResult(&summary, row)
	}
	if asJSON {
		err = json.NewEncoder(a.out).Encode(summary)
	} else {
		err = printSaidEval(a.out, summary)
	}
	if err != nil {
		return err
	}
	if len(groups) > 0 && summary.Errors == len(groups) {
		return errors.New("said eval: все случаи завершились ошибкой")
	}
	return nil
}

func readSaidEvalPrompt(path string) (string, error) {
	if path == "" {
		return said.Prompt, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("said eval prompt: %w", err)
	}
	return string(b), nil
}

func (a *app) saidEvalGroup(ctx context.Context, c *config.Config, prompt string, prior []said.Entry,
	group saidEvalGroup) saidEvalResult {
	row := saidEvalResult{Ref: group.Ref, Expected: group.Expected}
	p, err := a.saidEvalPending(group.Ref, &c.Said)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	p.Attempts = 1
	cutoff := p.Event.At.Truncate(time.Second)
	refBase := evalRefBase(group.Ref)
	filtered := make([]said.Entry, 0, len(prior))
	for i := range prior {
		if prior[i].Time.Before(cutoff) && evalRefBase(prior[i].Ref) != refBase {
			filtered = append(filtered, prior[i])
		}
	}
	state := &said.State{}
	accounts, skipped := a.saidAccounts(c, state, time.Now())
	if len(accounts) == 0 {
		row.Error = "нет доступной учётки: " + strings.Join(skipped, "; ")
		return row
	}
	rec, _, _ := a.saidAskPrompt(ctx, &c.Said, state, &accounts, &p, filtered, prompt)
	if rec.Err != "" {
		row.Error = rec.Err
		return row
	}
	row.Actual = rec.Items
	compareSaidEval(&row)
	return row
}

func evalRefBase(ref string) string {
	base, _, _ := strings.Cut(ref, "#")
	return base
}

func compareSaidEval(row *saidEvalResult) {
	expected := make([]saidEvalPair, 0, len(row.Expected))
	for _, pair := range row.Expected {
		if pair.Kind != "-" {
			expected = append(expected, pair)
		}
	}
	row.ExpectedN, row.FoundN = len(expected), len(row.Actual)
	matched := make([]int, len(row.Actual))
	for i := range matched {
		matched[i] = -1
	}
	for i := range expected {
		seen := make([]bool, len(row.Actual))
		augmentEvalMatch(i, expected, row.Actual, matched, seen)
	}
	for _, expectedIndex := range matched {
		if expectedIndex >= 0 {
			row.MatchedN++
		}
	}
	row.Extra = max(0, row.FoundN-row.MatchedN)
}

func augmentEvalMatch(expectedIndex int, expected []saidEvalPair, actual []said.Item, matched []int, seen []bool) bool {
	for actualIndex := range actual {
		if seen[actualIndex] || !evalPairMatches(expected[expectedIndex], actual[actualIndex]) {
			continue
		}
		seen[actualIndex] = true
		if matched[actualIndex] == -1 || augmentEvalMatch(matched[actualIndex], expected, actual, matched, seen) {
			matched[actualIndex] = expectedIndex
			return true
		}
	}
	return false
}

func evalPairMatches(expected saidEvalPair, actual said.Item) bool {
	return actual.Kind == expected.Kind && (expected.Scope == "-" || actual.Scope == expected.Scope)
}

func addSaidEvalResult(summary *saidEvalSummary, row saidEvalResult) {
	if row.Error != "" {
		summary.Errors++
		return
	}
	summary.ExpectedN += row.ExpectedN
	summary.FoundN += row.FoundN
	summary.MatchedN += row.MatchedN
	summary.ExtraRecords += row.Extra
}

func printSaidEval(out io.Writer, summary saidEvalSummary) error {
	for _, row := range summary.Cases {
		if row.Error != "" {
			if _, err := fmt.Fprintf(out, "%s\tошибка: %s\n", row.Ref, row.Error); err != nil {
				return fmt.Errorf("said eval вывод случая %s: %w", row.Ref, err)
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%s\tожидание [%s]\tответ [%s]\tожидалось %d, пришло %d, совпало %d, лишних %d\n",
			row.Ref, formatEvalExpected(row.Expected), formatEvalActual(row.Actual), row.ExpectedN, row.FoundN,
			row.MatchedN, row.Extra); err != nil {
			return fmt.Errorf("said eval вывод случая %s: %w", row.Ref, err)
		}
	}
	_, err := fmt.Fprintf(out, "итог: ожидалось %d, найдено %d, совпало %d, лишних записей %d, ошибок %d\n",
		summary.ExpectedN, summary.FoundN, summary.MatchedN, summary.ExtraRecords, summary.Errors)
	return err
}

func formatEvalExpected(pairs []saidEvalPair) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, pair.Kind+"/"+pair.Scope)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

func formatEvalActual(items []said.Item) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.Kind+"/"+item.Scope)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

func (a *app) saidEvalPending(ref string, cfg *said.Config) (said.Pending, error) {
	id, line, err := parseSaidEvalRef(ref)
	if err != nil {
		return said.Pending{}, err
	}
	paths, err := filepath.Glob(filepath.Join(a.projects, "*", id+"*.jsonl"))
	if err != nil {
		return said.Pending{}, fmt.Errorf("поиск транскрипта %s: %w", ref, err)
	}
	if len(paths) == 0 {
		return said.Pending{}, fmt.Errorf("транскрипт для %s не найден", ref)
	}
	if len(paths) > 1 {
		return said.Pending{}, fmt.Errorf("для %s найдено несколько транскриптов", ref)
	}
	tail := said.NewTail(a.projects)
	tail.Only = paths[0]
	state := &tailpkg.State{Files: map[string]*tailpkg.File{}, Started: true}
	events, passErr := tail.Pass(state, time.Now()) // как демон: ошибки отдельных строк не мешают другим событиям
	if passErr != nil {
		a.warn("said", "часть транскрипта не прочиталась", "ref", ref, "err", passErr)
	}
	if err := a.saidNames(events); err != nil {
		return said.Pending{}, err
	}
	collected := &said.State{Last: map[string]string{}}
	for _, event := range events {
		before := len(collected.Pending)
		cfg.Collect(collected, []said.Event{event})
		if event.Line == line {
			if event.Kind != "user" || len(collected.Pending) == before {
				return said.Pending{}, fmt.Errorf("ref %s не указывает на наблюдаемую реплику пользователя", ref)
			}
			return collected.Pending[len(collected.Pending)-1], nil
		}
	}
	return said.Pending{}, fmt.Errorf("строка ref %s не найдена", ref)
}

func parseSaidEvalRef(ref string) (string, int64, error) {
	id, lineText, ok := strings.Cut(ref, ":")
	if !ok || len(id) != 8 || strings.Contains(lineText, ":") {
		return "", 0, fmt.Errorf("неверный ref %q", ref)
	}
	line, err := strconv.ParseInt(lineText, 10, 64)
	if err != nil || line < 1 {
		return "", 0, fmt.Errorf("неверная строка ref %q", ref)
	}
	return id, line, nil
}
