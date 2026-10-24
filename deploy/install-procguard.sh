#!/bin/sh
# Сборка и установка procguard (только user-часть, без sudo).
set -eu
cd "$(dirname "$0")/.."
go build -p 4 -trimpath -o bin/procguard ./cmd/procguard
install -D -m 0755 bin/procguard "$HOME/.local/bin/procguard"
install -D -m 0644 deploy/procguard.service "$HOME/.config/systemd/user/procguard.service"
[ -f "$HOME/.config/procguard/config.toml" ] || install -D -m 0644 deploy/procguard.example.toml "$HOME/.config/procguard/config.toml"
systemctl --user daemon-reload
systemctl --user enable procguard.service
systemctl --user restart procguard.service
