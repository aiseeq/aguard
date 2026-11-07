#!/bin/bash
# Приёмка тикета daemon-path-after-boot после перезагрузки: PATH процесса, вызовы said и WARN/ERROR от старта службы.
p=$(systemctl --user show -p MainPID --value aguard)
echo "MainPID=$p; $(systemctl --user show aguard -p ActiveEnterTimestamp)"
echo "Загрузка: $(uptime -s)"
echo "PATH процесса:"; tr '\0' '\n' </proc/$p/environ | grep ^PATH=
echo "Вызовы said после старта:"; jq -r --arg t "$(date -d "$(systemctl --user show aguard -p ActiveEnterTimestamp --value)" -Iseconds)" 'select(.at>$t) | [.at[11:19], .account, (.err//"ok")]|@tsv' ~/.local/state/aguard/said/$(date +%F).jsonl
echo "WARN/ERROR после старта:"; journalctl --user -u aguard.service --since "$(systemctl --user show aguard -p ActiveEnterTimestamp --value)" --no-pager | grep -E 'level=(WARN|ERROR)' | cut -c30-250
echo "Строк said.tsv: $(wc -l < ~/.claude/said.tsv); очередь: $(jq -r '.pending|length' ~/.local/state/aguard/said/state.json)"
