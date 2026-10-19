# cl

Короткий запуск Claude Code: `cl [аргументы claude]` = `claude --dangerously-skip-permissions -n <имя> [аргументы]`.

Имя сессии — по папке запуска: строка таблицы `~/.config/cl/names.tsv` («папка<TAB>имя», `~` раскрывается),
иначе `<папка>-master` для `~/git/<папка>` и `~/work/<папка>`, иначе без имени. Имя занято живой сессией
(реестр `~/.claude/sessions/*.json`) — `-2`, `-3`… Свой `-n`/`--name`, `-p` и подкоманды (`cl mcp list`) — как у claude.

Сессия идёт в своём scope `agent-<имя>-<pid>` слайса `agents.slice` (`systemd-run --user --scope`, тот же процесс:
терминал, Ctrl+C и код выхода как у голого claude). Слайс — `agents.slice` в репо: CPUWeight и IOWeight 40 против
100 у приложений пользователя, лимитов памяти нет. Без файла слайса или без systemd-run cl отказывается с ошибкой.

Установка: `go build -o ~/bin/cl .`, `cp agents.slice ~/.config/systemd/user/ && systemctl --user daemon-reload`.
Проверка: `cat /proc/<pid claude>/cgroup` → `…/agents.slice/agent-<имя>-<pid>.scope`.
