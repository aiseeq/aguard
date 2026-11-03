#!/bin/sh
# Один раз под sudo: ставит помощника, проверяет передачу дескрипторов и только потом снимает старые root-службы.
# Проверка не прошла — старые службы работают как работали. Вызов: graft install-helper.
set -eu
user=$1
bin=$2
uid=$(id -u "$user")
install -m 0755 "$bin/aguard-track-helper" /usr/local/sbin/aguard-track-helper
sed "s#@UID@#$uid#g" "$(dirname "$0")/aguard-track-helper.service" >/etc/systemd/system/aguard-track-helper.service
chmod 0644 /etc/systemd/system/aguard-track-helper.service
systemctl daemon-reload
systemctl enable aguard-track-helper.service
systemctl restart aguard-track-helper.service
i=0
until [ -S /run/aguard-track/fds.sock ]; do
	i=$((i + 1))
	[ "$i" -le 50 ] || { echo "помощник не поднял сокет" >&2; exit 1; }
	sleep 0.1
done
runuser -u "$user" -- "$bin/aguard" track check
systemctl disable --now tmpguard-track.service tmpguard-track-home.service
rm -f /etc/systemd/system/tmpguard-track.service /etc/systemd/system/tmpguard-track-home.service /usr/local/sbin/tmpguard-track
systemctl daemon-reload
# Снимки и журнал читает и забирает пользователь; пустые папки /var/lib останутся у него, sudo на их уборку не нужен.
chown -R "$user" /var/lib/tmpguard /var/lib/tmpguard-home
