// Package guide is the source of truth for session letters, interruptions and
// the channels used by aguard.
package guide

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type Kind string

type Quiet string

const (
	QuietWait Quiet = "ждёт конца aguard quiet; повторная попытка — при следующем проходе, пока событие актуально"
	QuietPass Quiet = "идёт сквозь aguard quiet"
	QuietLose Quiet = "теряется в тишине; автоматического повтора нет, новое письмо возможно при новом событии"
)

type Letter struct {
	Kind                     Kind
	Observer, To, When, Next string
	Quiet                    Quiet
	QuietDetail              string
}

type Interruption struct {
	ID, What, When, To, Config string
}

var letters = []Letter{
	{Kind: "bgtask.stalled", Observer: "bgtask", To: "владелец фоновой задачи", When: "обычная команда, уведённая Claude Code по таймауту, долго работает без нового вывода", Next: "проверьте, нужна ли задача; если нет — `TaskStop <id>` из письма", Quiet: QuietLose},
	{Kind: "bgtask.wait", Observer: "bgtask", To: "владелец задачи", When: "намеренное фоновое ожидание длится дольше часа", Next: "проверьте, нужен ли новый срок; если нет — `TaskStop <id>` из письма", Quiet: QuietLose},
	{Kind: "bgtask.other", Observer: "bgtask", To: "получатель уведомления", When: "получен неизвестный тип уведомления фоновой задачи; причина в a.log", Next: "проверьте тип уведомления и текст в a.log", Quiet: QuietLose},
	{Kind: "remote.owner", Observer: "remote", To: "владелец задачи", When: "graft-задача на PC не дошла и выполнена на ноуте", Next: "проверьте результат выполнения на ноуте и причину сбоя из письма", Quiet: QuietLose},
	{Kind: "remote.copy", Observer: "remote", To: "лид", When: "первый сбой удалённого пути за окно повтора", Next: "проверьте причину сбоя и доступность PC из письма", Quiet: QuietLose},
	{Kind: "remote.summary", Observer: "remote", To: "лид", When: "путь на PC сломан у нескольких мастеров", Next: "проверьте общую причину и доступность указанного PC", Quiet: QuietLose},
	{Kind: "remote.other", Observer: "remote", To: "получатель уведомления", When: "получен неизвестный тип сбоя удалённого пути; причина в a.log", Next: "проверьте уведомление и формат его ключа в a.log", Quiet: QuietLose},
	{Kind: "orphans.summary", Observer: "orphans", To: "администратор пространства или лид", When: "процессы Claude Code пережили сессию дольше min_dead и были сняты", Next: "если процесс нужен, запускайте его в systemd-run/graft start или добавьте исключение в конфиг; отвечать на письмо не нужно", Quiet: QuietLose},
	{Kind: "night.dream", Observer: "night", To: "лид и dream_to", When: "Сон не оставил маркер к dream_by, до конца окна запасного запуска", Next: "проверьте тикет dream-run и маркер; если запуск не идёт — запустите Сон по скиллу dream", Quiet: QuietWait},
	{Kind: "night.dream_pause", Observer: "night", To: "лид и dream_to", When: "файл паузы Сна не разобран", Next: "исправьте или удалите файл паузы по `~/git/dream/docs/night-pause.md`", Quiet: QuietWait},
	{Kind: "night.digest", Observer: "night", To: "лид", When: "после 07:00 в маркере Дайджеста нет сегодняшней даты", Next: "запустите Дайджест по скиллу digest", Quiet: QuietWait},
	{Kind: "night.trust", Observer: "night", To: "лид", When: "окно доверия не подтверждено за trust_wait или стоит на нём", Next: "откройте консоль названной сессии; подтвердите стрелкой вниз и Enter либо разберите её состояние", Quiet: QuietWait},
	{Kind: "night.fallback_blocked", Observer: "night", To: "получатели Сна", When: "не удалось записать состояние перед запасным запуском", Next: "проверьте запись состояния; если Сон нужен сейчас — запустите его вручную", Quiet: QuietWait},
	{Kind: "night.fallback", Observer: "night", To: "получатели Сна", When: "запасной Сон запущен или запуск не вышел", Next: "проверьте unit через `systemctl --user status` и журнал; при сбое запустите команду из письма", Quiet: QuietWait},
	{Kind: "night.fallback_end", Observer: "night", To: "получатели Сна", When: "запасной unit завершился или пропал", Next: "проверьте код и хвост через `journalctl --user -u <unit>`", Quiet: QuietWait},
	{Kind: "night.fallback_state", Observer: "night", To: "получатели Сна", When: "loadFallbackState не прочитал fallback.json", Next: "почините или уберите fallback.json по тексту письма", Quiet: QuietWait},
	{Kind: "night.other", Observer: "night", To: "получатели сигнала", When: "получен неизвестный ключ ночного сигнала; точная причина записана в журнале демона", Next: "проверьте ключ и сообщение в a.log; добавьте известный формат в отображение", Quiet: QuietWait},
	{Kind: "disk.low_free", Observer: "disk", To: "лид и free_tell", When: "свободное место ниже disk.free_min", Next: "освободите место и проверьте `aguard disk report`", Quiet: QuietWait},
	{Kind: "disk.fast", Observer: "disk", To: "владелец папки или лид", When: "свободное место быстро заканчивается", Next: "сверьте названные папки и освободите место; `aguard disk report`", Quiet: QuietLose, QuietDetail: "в тишине письмо пропадает: отметка ставится до отправки; следующее возможно не раньше fast_every, если быстрый расход сохранится"},
	{Kind: "disk.forecast", Observer: "disk", To: "лид и free_tell", When: "по темпу роста прогнозируется заполнение диска", Next: "проверьте растущие пути через `aguard disk report` и освободите место", Quiet: QuietWait},
	{Kind: "names.broken", Observer: "names_table", To: "лид", When: "таблица имён не читается или повреждена", Next: "исправьте указанную строку таблицы; следующий проход перечитает файл", Quiet: QuietWait},
	{Kind: "io.machine", Observer: "io_route", To: "лид", When: "обнаружен общий ущерб машины от фризов", Next: "изучите `aguard io report -since 168h` и разберите главные cgroup", Quiet: QuietLose},
	{Kind: "io.urgent", Observer: "io_route", To: "владельцу виновника", When: "вред продолжается сейчас у хозяина", Next: "проверьте нагрузку и `aguard io report -since 168h`", Quiet: QuietLose},
	{Kind: "io.cross", Observer: "io_route", To: "лид", When: "один класс фризит у разных хозяев", Next: "сверьте тикеты `io-freeze-*` и общую причину класса", Quiet: QuietLose},
	{Kind: "io.offline", Observer: "io_route", To: "лид", When: "срочный вред продолжается, а хозяин не в сети", Next: "проверьте `aguard io report -since 168h` и тикет хозяина", Quiet: QuietLose},
	{Kind: "io.victim", Observer: "io_route", To: "лид", When: "возможная жертва вытеснения памяти вызывает фриз", Next: "проверьте cgroup и тикет владельца; подробности — `aguard io report -since 168h`", Quiet: QuietLose},
	{Kind: "io.no_owner", Observer: "io_route", To: "лид", When: "срочный вред есть, но хозяин не установлен", Next: "найдите виновника через `aguard io report -since 168h`", Quiet: QuietLose},
	{Kind: "io.after_pause", Observer: "io_route", To: "тот, чья пауза писем закончилась", When: "фризы продолжаются после паузы писем", Next: "изучите `aguard io report -since 168h`", Quiet: QuietLose},
	{Kind: "io.other", Observer: "io_route", To: "получатель маршрута", When: "получена неизвестная причина io; исходное значение записано в a.log", Next: "проверьте случай через `aguard io report -since 168h` и исходное значение в a.log", Quiet: QuietLose},
	{Kind: "codex.warn", Observer: "codex", To: "лид", When: "по темпу учётка достигнет 100% до планового сброса в пределах warn", Next: "решите, кто вызывает consume при 100%: вы сами или aguard после «да» в approvals-файле; проверьте исход из следующего письма", Quiet: QuietWait},
	{Kind: "codex.full", Observer: "codex", To: "лид", When: "учётка достигла 100% и обычная работа запрещена либо взвод не одобрен", Next: "дождитесь сброса или разберите одобрение и исход consume в письме", Quiet: QuietWait},
	{Kind: "codex.blocked", Observer: "codex", To: "лид", When: "поставщик запретил обычную работу до 100%", Next: "проверьте статус лимита и дождитесь разрешения поставщика", Quiet: QuietWait},
	{Kind: "codex.all_out", Observer: "codex", To: "лид", When: "все учётки Codex исчерпали лимит", Next: "перенесите работу до сброса одной из учёток", Quiet: QuietWait},
	{Kind: "codex.other", Observer: "codex", To: "лид", When: "получен неизвестный тип уведомления codex; подробности в a.log", Next: "проверьте исходный тип и текст уведомления в a.log", Quiet: QuietLose},
	{Kind: "codex.outcome", Observer: "codex", To: "лид", When: "получен исход попытки досрочного consume", Next: "следуйте указанному в письме исходу consume", Quiet: QuietWait},
	{Kind: "tmp.summary", Observer: "tmp", To: "владельцы записей", When: "сводка объёма временных файлов владельца готова", Next: "разберите перечисленные записи; `aguard tmp report` покажет историю", Quiet: QuietWait},
	{Kind: "tmp.test", Observer: "tmp", To: "адресат `aguard tmp send-test`", When: "пользователь вручную запросил тестовую сводку", Next: "проверьте сокет сессии; повторите `aguard tmp send-test` после окончания тишины, если нужно", Quiet: QuietLose},
	{Kind: "cpu.owner", Observer: "cpu", To: "мастеров с наибольшей долей готовых задач", When: "перегрузка длится дольше cpu.after и мастер достиг срока повтора", Next: "проверьте свои готовые задачи и процессы; отложите тяжёлую работу", Quiet: QuietWait},
	{Kind: "cpu.summary", Observer: "cpu", To: "лид", When: "перегрузку нельзя отнести к одному мастеру или мастеров несколько", Next: "изучите сводку хозяев и распределение готовых задач в письме", Quiet: QuietWait},
	{Kind: "cpu.stray", Observer: "cpu", To: "мастер процесса без родителя или лид", When: "обнаружен достаточно старый процесс без родителя", Next: "если процесс не нужен — снимите дерево (TERM, затем KILL при необходимости); если нужен в фоне — запустите службой `systemd-run --user`", Quiet: QuietWait},
	{Kind: "cpu.stray_copy", Observer: "cpu", To: "лид", When: "процесс без родителя повторно сообщён, а мастер его не снял", Next: "проверьте PID и решите, нужно ли останавливать процесс", Quiet: QuietWait},
	{Kind: "ceiling.oom", Observer: "ceiling", To: "сессия-жертва или лид", When: "ядро сняло процессы по MemoryMax/OOM", Next: "проверьте перечисленные процессы; при необходимости поднимите лимит через `aguard memory ceiling <N>`", Quiet: QuietPass},
	{Kind: "sigguard.denied", Observer: "sigguard", To: "владелец задания или лид", When: "agent-signal-guard запретил сигнал чужому unit", Next: "найдите в письме, что послало сигнал", Quiet: QuietPass},
	{Kind: "sigguard.unparsed", Observer: "sigguard", To: "лид", When: "журнал отказов сигнала не удалось разобрать", Next: "разберите первую исходную строку из письма", Quiet: QuietPass},
	{Kind: "said.stalled", Observer: "said", To: "лид", When: "модель обработки реплик не отвечает дольше said.stall", Next: "выполните `aguard said report`; проверьте зависшую обработку", Quiet: QuietWait},
	{Kind: "queue.crit", Observer: "queue", To: "первому живому из мастера, администратора и лида", When: "открыт тикет CRIT", Next: "бросьте текущее, кроме другого CRIT: `tk show <slug>` (внешний тикет: `tk next`)", Quiet: QuietWait},
	{Kind: "queue.crit_copy", Observer: "queue", To: "лид", When: "мастер не взял CRIT или недоступен", Next: "проверьте `tk show <slug>` и связь с мастером", Quiet: QuietWait},
	{Kind: "queue.high", Observer: "queue", To: "свободному мастеру", When: "открытый HIGH доступен в очереди", Next: "выполните `tk next`", Quiet: QuietWait},
	{Kind: "queue.high_admin", Observer: "queue", To: "администратор или лид", When: "у HIGH нет живого мастера либо он не взят четыре часа", Next: "проверьте `tk show <slug>` и назначение мастера", Quiet: QuietWait},
	{Kind: "queue.wake", Observer: "queue", To: "свободному мастеру", When: "у мастера появилась работа в `tk next`", Next: "выполните `tk next`; если первый тикет нельзя взять — задайте `tk wait` с причиной и сроком", Quiet: QuietWait},
	{Kind: "queue.wake_stuck", Observer: "queue", To: "администратор или лид", When: "мастер получил будильник, но очередь осталась", Next: "проверьте первый тикет и ответьте мастеру", Quiet: QuietWait},
	{Kind: "queue.wake_admin", Observer: "queue", To: "администратор проекта", When: "живого мастера нет, администратору доступна работа", Next: "выполните указанную команду `cd … && tk next --by …` в отдельном дереве", Quiet: QuietWait},
	{Kind: "queue.back", Observer: "queue", To: "автору закрытого тикета", When: "назначенный тикет закрыт или перенесён", Next: "проверьте результат тикета и при необходимости продолжите его", Quiet: QuietWait},
	{Kind: "queue.back_lost", Observer: "queue", To: "письма нет, только журнал", When: "закрытый тикет не удалось сообщить автору за семь суток", Next: "проверьте журнал очереди при необходимости", Quiet: QuietLose, QuietDetail: "не применяется: письма нет, только журнал"},
	{Kind: "queue.unreachable", Observer: "queue", To: "письма нет, только журнал", When: "по тикету очереди некому отправить уведомление", Next: "проверьте журнал очереди и доступность адресатов", Quiet: QuietLose, QuietDetail: "не применяется: письма нет, только журнал"},
	{Kind: "queue.other", Observer: "queue", To: "получатель уведомления", When: "получен неизвестный вид уведомления очереди; исходное значение в журнале очереди и a.log при отправке", Next: "проверьте исходное значение в журнале очереди или a.log", Quiet: QuietWait},
}

var interruptions = []Interruption{
	{ID: "orphans.stop", What: "деревья процессов умершей сессии; контейнер целиком", When: "режим act, процесс пережил сессию дольше min_dead и не исключён", To: "сводка администратору/лиду", Config: "orphans.mode=act, min_dead, exclude, mute_until"},
	{ID: "orphans.term", What: "SIGTERM процессам дерева умершей сессии", When: "режим act прошёл планирование; pidfd подтверждает исходный процесс", To: "сводка по завершении снятия", Config: "orphans.mode=act, term_wait"},
	{ID: "orphans.kill", What: "SIGKILL процессам дерева, не вышедшим после ожидания", When: "SIGTERM не завершил процесс за term_wait", To: "сводка по завершении снятия", Config: "orphans.mode=act, term_wait"},
	{ID: "orphans.container", What: "контейнер умершей сессии через podman stop", When: "режим act выбрал дерево контейнера", To: "сводка по завершении снятия", Config: "orphans.mode=act, term_wait"},
	{ID: "sigguard.deny", What: "сигнал процессу внутри защищённого unit", When: "agent-signal-guard запретил сигнал, адресованный чужому unit", To: "владелец задания/лид письмом sigguard.denied", Config: "политика службы agent-signal-guard; sigguard.mode=alert включает письмо"},
	{ID: "ceiling.oom", What: "процессы в cgroup сессии", When: "ядро достигло MemoryMax и OOM killer снял процессы", To: "целевая сессия/лид письмом ceiling.oom", Config: "MemoryMax из настройки сессии cl; ceiling.interval задаёт частоту наблюдения"},
	{ID: "night.fallback", What: "завершённый запасной unit Сна снимается systemctl stop", When: "команда завершилась или unit перешёл в failed; active/exited освобождается, чтобы не держать unit до перезагрузки", To: "сессиям ночного сигнала приходит отчёт об исходе", Config: "night.dream_fallback, night.dream_fallback_by"},
	{ID: "io.service", What: "импорт сводок io отклонён", When: "демон aguard запущен и мог бы перезаписать сводки", To: "пользователь команды получает отказ с просьбой остановить службу", Config: "нет; остановите `systemctl --user stop aguard` вручную"},
	{ID: "io.dump_signal", What: "служба aguard получает SIGUSR2 для ручного снимка io", When: "пользователь запускает `aguard io dump`", To: "письма нет; команда ждёт файл снимка", Config: "нет"},
	{ID: "cl.supervise", What: "watch по ответу aguard restart в режиме act запускает перезапуск; если `claude … /compact` не завершился за 10 минут, compactSession снимает процесс через StopChild", When: "watch получил restart от aguard при действующих restart.mode, compact_mode/reset_mode или idle; /compact превысил константу compactTimeout=10m", To: "письма нет; результат записывается в журнал cl", Config: "restart.mode=act, compact_mode, reset_mode, idle; compactTimeout=10m — константа"},
	{ID: "cl.signal", What: "дочерний Claude получает SIGTERM при сигнале обёртке", When: "cl получил SIGTERM или SIGHUP", To: "письма нет", Config: "cl supervise"},
	{ID: "cl.console", What: "только что запущенный Claude снимается StopChild", When: "подключение консоли к pty не удалось", To: "письма нет", Config: "cl console"},
	{ID: "track.check.cleanup", What: "дочерний процесс временной проверки track", When: "проверка завершилась или обнаружила ошибку и чистит тестовый процесс", To: "письма нет", Config: "команда `aguard track check`"},
	{ID: "child.timeout", What: "дочерние команды сторожа снимаются по сроку контекста; WaitDelay завершает оставшийся процесс. Места: cmd/aguard diskFacts, findClassTicket, tkRun, appServer, noteTicket, startJournal, ceilingJournalCursor, cpuRemote, queueTicketNote, queueTickets, nextForQueue, workerOwners, readQueueExternal, readStrays, nightPauseHolds; internal/guiwatch journalWindow/kernelJournalWindow; internal/orphans podmanStop", When: "истёк контекст команды", To: "письма нет; вызывающая команда получает ошибку таймаута", Config: "таймауты queueExternalTimeout, codexReadTimeout, sshTimeout и родительские контексты"},
}

func Letters() []Letter             { return append([]Letter(nil), letters...) }
func Interruptions() []Interruption { return append([]Interruption(nil), interruptions...) }
func Lookup(k Kind) (Letter, bool) {
	for _, x := range letters {
		if x.Kind == k {
			return x, true
		}
	}
	return Letter{}, false
}
func Known() []string {
	out := make([]string, 0, len(letters))
	seen := map[string]bool{}
	for _, x := range letters {
		out = append(out, string(x.Kind))
		seen[x.Observer] = true
	}
	for observer := range seen {
		out = append(out, observer)
	}
	slices.Sort(out)
	return out
}

func ByName(name string) ([]Letter, bool) {
	if x, ok := Lookup(Kind(name)); ok {
		return []Letter{x}, true
	}
	var out []Letter
	for _, x := range Letters() {
		if x.Observer == name {
			out = append(out, x)
		}
	}
	return out, len(out) > 0
}

func Render() string {
	var b strings.Builder
	groups := map[string][]Letter{}
	for _, x := range letters {
		groups[x.Observer] = append(groups[x.Observer], x)
	}
	names := slices.Sorted(maps.Keys(groups))
	b.WriteString("Письма сторожа\n")
	for _, n := range names {
		fmt.Fprintf(&b, "\n%s:\n", n)
		for _, x := range groups[n] {
			renderLetter(&b, x)
		}
	}
	b.WriteString("\nПрерывания\n")
	for _, x := range Interruptions() {
		fmt.Fprintf(&b, "- %s — %s; условие: %s; письмо: %s; конфиг: %s\n", x.ID, x.What, x.When, x.To, x.Config)
	}
	b.WriteString("\nКак сторож сообщает\n- Сессии: только сокет сессии через deliver.Send.\n- Демон: журнал a.log.\n- Состояние: файлы состояния наблюдателей.\n- Telegram, почта и другие пользовательские каналы не используются.\n")
	return b.String()
}

func RenderLetters(xs []Letter) string {
	var b strings.Builder
	for _, x := range xs {
		renderLetter(&b, x)
	}
	return b.String()
}
func renderLetter(b *strings.Builder, x Letter) {
	quiet := string(x.Quiet)
	if x.QuietDetail != "" {
		quiet = x.QuietDetail
	}
	fmt.Fprintf(b, "- %s — кому: %s; когда: %s; дальше: %s; тишина: %s.\n", x.Kind, x.To, x.When, x.Next, quiet)
}
