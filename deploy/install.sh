#!/bin/sh
# Сборка и установка aguard: user-служба aguard, запуск сессий cl и root-службы tmpguard-track (/tmp) и
# tmpguard-track-home (папки следов в ~/.cache и ~/.local/state).
# Только user-часть: deploy/install.sh; вместе с tmpguard-track (sudo): deploy/install.sh --track.
set -eu
cd "$(dirname "$0")/.."
go build -trimpath -o bin/aguard ./cmd/aguard
go build -trimpath -o bin/cl ./cmd/cl
if [ "${1:-}" = "--track" ]; then
	go build -trimpath -o bin/tmpguard-track ./cmd/tmpguard-track
	sudo install -m 0755 bin/tmpguard-track /usr/local/sbin/tmpguard-track
	for unit in tmpguard-track tmpguard-track-home; do
		sudo install -m 0644 "deploy/$unit.service" "/etc/systemd/system/$unit.service"
	done
	sudo systemctl daemon-reload
	sudo systemctl enable tmpguard-track.service tmpguard-track-home.service
	sudo systemctl restart tmpguard-track.service tmpguard-track-home.service
fi
install -D -m 0755 bin/aguard "$HOME/.local/bin/aguard"
install -D -m 0644 deploy/aguard.service "$HOME/.config/systemd/user/aguard.service"
[ -f "$HOME/.config/aguard/config.toml" ] || install -D -m 0644 deploy/config.example.toml "$HOME/.config/aguard/config.toml"
# cl — через новый файл и mv: запущенные консоли держат старый и не задеваются.
install -D -m 0755 bin/cl "$HOME/bin/cl.new"
mv "$HOME/bin/cl.new" "$HOME/bin/cl"
[ -f "$HOME/.config/cl/config.json" ] || install -D -m 0644 cmd/cl/config.json "$HOME/.config/cl/config.json"
install -D -m 0644 cmd/cl/agents.slice "$HOME/.config/systemd/user/agents.slice"
systemctl --user daemon-reload
systemctl --user enable aguard.service
systemctl --user restart aguard.service
