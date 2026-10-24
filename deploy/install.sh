#!/bin/sh
# Сборка и установка tmpguard: root-часть (system) и user-часть (aisee).
set -eu
cd "$(dirname "$0")/.."
go build -p 4 -trimpath -o bin/tmpguard-track ./cmd/tmpguard-track
go build -p 4 -trimpath -o bin/tmpguard ./cmd/tmpguard

sudo install -m 0755 bin/tmpguard-track /usr/local/sbin/tmpguard-track
sudo install -m 0644 deploy/tmpguard-track.service /etc/systemd/system/tmpguard-track.service
sudo systemctl daemon-reload
sudo systemctl enable tmpguard-track.service
sudo systemctl restart tmpguard-track.service

install -D -m 0755 bin/tmpguard "$HOME/.local/bin/tmpguard"
install -D -m 0644 deploy/tmpguard.service "$HOME/.config/systemd/user/tmpguard.service"
[ -f "$HOME/.config/tmpguard/config.toml" ] || install -D -m 0644 deploy/config.example.toml "$HOME/.config/tmpguard/config.toml"
systemctl --user daemon-reload
systemctl --user enable tmpguard.service
systemctl --user restart tmpguard.service
