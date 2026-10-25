// Наблюдатель restart: aguard отвечает cl в сокет, можно ли сейчас перезапустить сессию на новую версию Claude Code.
// Перезапускает cl; без ответа aguard cl не перезапускает.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/aiseeq/aguard/internal/restart"
)

func (a *app) restartCmd(args []string) error {
	cmd, rest := sub(args)
	if cmd != "ask" {
		usage()
	}
	fl := flag.NewFlagSet("restart ask", flag.ContinueOnError)
	pid := fl.Int("pid", 0, "процесс claude")
	name := fl.String("name", "", "имя сессии, если реестр его не знает")
	claude := fl.String("claude", "", "другая ссылка на установленный claude: проверить решение, как при выходе новой версии")
	parse(fl, rest)
	ans := a.restartAnswer(restart.NewJudge(a.home), restart.Request{V: restart.Version, Pid: *pid, Name: *name}, *claude)
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&ans); err != nil {
		return fmt.Errorf("вывод: %w", err)
	}
	return nil
}

// restartAnswer — ответ по свежему конфигу; конфиг не прочитался — ответа нет, cl не перезапустит. claude — замена
// ссылки на установленный claude для ручной проверки, пусто — из конфига.
func (a *app) restartAnswer(j *restart.Judge, req restart.Request, claude string) restart.Answer {
	cfg, err := a.config()
	if err != nil {
		return restart.Answer{V: restart.Version, Error: "конфиг aguard: " + err.Error()}
	}
	if claude != "" {
		cfg.Restart.Claude = claude
	}
	return j.Answer(&cfg.Restart, req, time.Now())
}

// restartServe — сокет для cl до отмены ctx. Сокет берётся из конфига на старте демона: смена пути — после
// перезапуска aguard. Сокет не открылся — остальные наблюдатели работают, cl не получит ответа и не перезапустит.
func (a *app) restartServe(ctx context.Context) {
	cfg, err := a.config()
	if err != nil {
		a.log.Error("aguard restart: конфиг, сокет не открыт", "err", err)
		return
	}
	ln, err := restart.Listen(cfg.Restart.Socket)
	if err != nil {
		a.log.Error("aguard restart: сокет не открыт", "err", err)
		return
	}
	j := restart.NewJudge(a.home)
	answer := func(req restart.Request) restart.Answer { return a.restartAnswer(j, req, "") }
	if err := restart.Serve(ctx, ln, answer, a.log); err != nil {
		a.log.Error("aguard restart: сокет", "err", err)
	}
}
