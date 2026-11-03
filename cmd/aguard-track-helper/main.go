// aguard-track-helper — неизменная root-часть слежения за записями: открывает группу fanotify и сокет proc connector и
// отдаёт их демону aguard заданного uid (internal/trackfd). Вся логика — в aguard и обновляется без sudo; этот файл
// меняется только с отпечатком пользователя, поэтому в нём ничего лишнего.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aiseeq/aguard/internal/trackfd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aguard-track-helper:", err)
		os.Exit(1)
	}
}

func run() error {
	uid := flag.Int("uid", -1, "кому отдавать дескрипторы")
	flag.Parse()
	if *uid <= 0 {
		return errors.New("нужен -uid обычного пользователя")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return trackfd.Serve(ctx, trackfd.Socket, *uid, trackfd.OpenPair, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}
