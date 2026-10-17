# cl

Короткий запуск Claude Code: `cl [аргументы claude]` = `claude --dangerously-skip-permissions -n <имя> [аргументы]`.

Имя сессии — по папке запуска: строка таблицы `~/.config/cl/names.tsv` («папка<TAB>имя», `~` раскрывается),
иначе `master-<папка>` для `~/git/<папка>` и `~/work/<папка>`, иначе без имени. Имя занято живой сессией
(реестр `~/.claude/sessions/*.json`) — `-2`, `-3`… Свой `-n`/`--name`, `-p` и подкоманды (`cl mcp list`) — как у claude.

Сборка: `go build -o ~/bin/cl .`
