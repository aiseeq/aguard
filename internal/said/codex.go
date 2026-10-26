package said

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/akit/price"
)

// Временно, до тега akit с oneshot.Request.Schema и запуском codex без инструментов (просьба akit-master 26.10):
// тогда Codex заменяется на oneshot.Ask. Строка «ждёт» — в ~/.claude/improve/state/backlog.md.

// noTools — возможности codex, которые разметке не нужны: без них вход Luna 6.0K токенов против 12.8K (замер 26.10).
var noTools = []string{"apps", "browser_use", "browser_use_external", "computer_use", "image_generation",
	"multi_agent", "plugins", "skill_search", "shell_tool", "unified_exec", "view_image", "goals", "sleep_tool",
	"collaboration_modes", "tool_suggest", "in_app_browser", "mentions_v2", "hooks", "steer"}

// Codex — Model через `codex exec`: id модели, усилие, инструкция Prompt и схема Schema файлами в dir.
func Codex(id, effort, dir string) (Model, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("said: папка вызова: %w", err)
	}
	prompt, schema := filepath.Join(dir, "prompt.md"), filepath.Join(dir, "schema.json")
	if err := errors.Join(os.WriteFile(prompt, []byte(Prompt), 0o600),
		os.WriteFile(schema, []byte(Schema), 0o600)); err != nil {
		return nil, fmt.Errorf("said: инструкция модели: %w", err)
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config",
		"--ignore-rules", "--sandbox", "read-only", "-m", id, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort),
		"-c", `web_search="disabled"`, "-c", "project_doc_max_bytes=0", "-c", "include_apply_patch_tool=false",
		"-c", fmt.Sprintf("model_instructions_file=%q", prompt)}
	for _, f := range noTools {
		args = append(args, "--disable", f)
	}
	args = append(args, "--output-schema", schema, "-")
	return func(ctx context.Context, input string) (string, price.Usage, error) {
		cmd := exec.CommandContext(ctx, "codex", args...)
		cmd.Dir = dir
		cmd.WaitDelay = 2 * time.Second // потомки codex держат stdout после отмены
		cmd.Stdin = strings.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			s := []rune(strings.TrimSpace(stderr.String()))
			return "", price.Usage{}, fmt.Errorf("codex: %w: %s", err, string(s[max(0, len(s)-300):]))
		}
		return codexAnswer(out)
	}, nil
}

// codexAnswer — последнее сообщение агента и расход из событий `codex exec --json`.
func codexAnswer(out []byte) (string, price.Usage, error) {
	var msg string
	var u price.Usage
	var failed []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<16), 16<<20)
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Item    struct {
				Type, Text string
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Usage struct {
				Input  int64 `json:"input_tokens"`
				Cached int64 `json:"cached_input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "item.completed":
			if e.Item.Type == "agent_message" {
				msg = e.Item.Text
			}
		case "turn.completed":
			u.Input += e.Usage.Input - e.Usage.Cached
			u.CacheRead += e.Usage.Cached
			u.Output += e.Usage.Output
		case "turn.failed":
			failed = append(failed, e.Error.Message)
		case "error":
			failed = append(failed, e.Message)
		}
	}
	switch {
	case sc.Err() != nil:
		return "", u, fmt.Errorf("codex: вывод: %w", sc.Err())
	case len(failed) > 0:
		return "", u, fmt.Errorf("codex: %s", strings.Join(failed, "; "))
	case msg == "":
		return "", u, errors.New("codex: нет ответа агента")
	}
	return msg, u, nil
}

// ApprovedID — id модели по имени из ~/.config/models/approved (строки «<имя> <id>», # — комментарий).
func ApprovedID(home, name string) (string, error) {
	path := filepath.Join(home, ".config", "models", "approved")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("said: одобренные модели: %w", err)
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == name && !strings.HasPrefix(f[0], "#") {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("said: модели %q нет в %s", name, path)
}
