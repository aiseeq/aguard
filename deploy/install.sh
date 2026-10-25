#!/bin/sh
# Сборка и установка aguard: user-служба aguard и root-служба tmpguard-track.
# Только user-часть: deploy/install.sh; вместе с tmpguard-track (sudo): deploy/install.sh --track.
set -eu
cd "$(dirname "$0")/.."
go build -trimpath -o bin/aguard ./cmd/aguard
if [ "${1:-}" = "--track" ]; then
	go build -trimpath -o bin/tmpguard-track ./cmd/tmpguard-track
	sudo install -m 0755 bin/tmpguard-track /usr/local/sbin/tmpguard-track
	sudo install -m 0644 deploy/tmpguard-track.service /etc/systemd/system/tmpguard-track.service
	sudo systemctl daemon-reload
	sudo systemctl enable tmpguard-track.service
	sudo systemctl restart tmpguard-track.service
fi
install -D -m 0755 bin/aguard "$HOME/.local/bin/aguard"
install -D -m 0644 deploy/aguard.service "$HOME/.config/systemd/user/aguard.service"
[ -f "$HOME/.config/aguard/config.toml" ] || install -D -m 0644 deploy/config.example.toml "$HOME/.config/aguard/config.toml"
systemctl --user daemon-reload
systemctl --user enable aguard.service
systemctl --user restart aguard.service
