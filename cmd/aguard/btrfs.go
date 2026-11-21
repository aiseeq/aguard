package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/btrfswatch"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/guide"
)

func (a *app) btrfsRound(ctx context.Context, c *config.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	files, readErr := btrfswatch.Read(c.Btrfs.SysfsRoot)
	var readFailures *btrfswatch.ReadError
	if readErr != nil && !errors.As(readErr, &readFailures) {
		return readErr
	}
	path := filepath.Join(a.dir("btrfs"), "sent.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("btrfs state directory: %w", err)
	}
	told, err := btrfswatch.Load(path)
	if err != nil {
		return fmt.Errorf("btrfs state: %w", err)
	}
	now := time.Now()
	active := map[string]bool{}
	to := append([]string{c.Btrfs.Lead}, c.Btrfs.Tell...)
	for _, f := range files {
		for _, al := range f.Alerts(c.Btrfs) {
			reference, err := btrfswatch.Reference()
			if err != nil {
				return err
			}
			for _, recipient := range to {
				key := f.UUID + "/" + al.Key + "/" + recipient
				active[key] = true
				if !told.Due(key, true, now) {
					continue
				}
				msg := fmt.Sprintf("aguard btrfs %s: %s. Замер: устройство %d байт, выделено %d байт, нераспределено %d байт, метаданные %d / %d байт, qgroups=%t. Справка: %s",
					f.UUID, al.Text, f.DeviceBytes, f.Allocated, f.Unallocated, f.MetadataUsed, f.MetadataTotal, f.Qgroups, reference)
				err := a.sendAliveFromBtrfs(recipient, msg, c.Btrfs.SendAuth)
				if err != nil && !a.quietFailure("btrfs", err) {
					a.warn("btrfs", "письмо о пороге", "session", recipient, "uuid", f.UUID, "threshold", al.Key, "err", err)
				}
				told.Mark(key, now, err == nil)
			}
		}
	}
	for key := range told {
		uuid, _, _ := strings.Cut(key, "/")
		if readFailures != nil && readFailures.Failed(uuid) {
			continue
		}
		if !active[key] {
			delete(told, key)
		}
	}
	if len(files) == 0 && readErr == nil { // Нет ФС: сбросить состояние текущего набора, чтобы последующее монтирование было новым сигналом.
		clear(told)
	}
	if err := told.Save(path); err != nil {
		return errors.Join(readErr, err)
	}
	return readErr
}

func (a *app) sendAliveFromBtrfs(to, msg string, auth bool) error {
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("btrfs", "реестр сессий", "err", err)
	}
	return a.sendAlive(alive, to, auth, btrfsGuideKind("threshold"), msg)
}

func btrfsGuideKind(_ string) guide.Kind { return "btrfs.threshold" }
