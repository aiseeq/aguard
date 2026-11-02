# aguard

`aguard` is a per-user daemon that watches Claude Code sessions and their child processes, along with the machine they run on. It records resource use and pressure, orphaned processes, `/tmp` usage, memory and disk growth, I/O and GUI stalls, forgotten background commands, agent waits and failures, context compaction, and scheduled overnight runs. It is designed to observe and report by default; it does not impose memory limits or routinely stop work. Alerts are sent as messages to agent sessions.

The daemon runs as a [systemd user service](deploy/aguard.service). Shared session and process handling uses [`github.com/aiseeq/akit`](go.mod). The design and implementation notes are in [docs/design.md](docs/design.md) and [docs/spec.md](docs/spec.md) (in Russian).

## Install

Requirements: Go 1.26.0 (the version declared in [go.mod](go.mod)) or newer, a running systemd user manager, and Linux with cgroup v2.

Build and install the binary, service unit, and example configuration:

```sh
go build -o bin/ ./cmd/...
install -D -m 0755 bin/aguard ~/.local/bin/aguard
install -D -m 0755 bin/cl ~/bin/cl                                   # optional session launcher
install -D -m 0644 deploy/aguard.service ~/.config/systemd/user/aguard.service
install -D -m 0644 cmd/cl/agents.slice ~/.config/systemd/user/agents.slice
[ -f ~/.config/aguard/config.toml ] || install -D -m 0644 deploy/config.example.toml ~/.config/aguard/config.toml
systemctl --user daemon-reload
systemctl --user enable --now aguard
```

The project’s [graft install task](.graft.yaml) also builds the tools and installs the user service; it preserves an existing configuration. `tmpguard-track`, which records `/tmp` ownership and requires root, is a separate optional system service and is not installed by the commands above.

## Commands

Run `aguard` without arguments to print the full usage, flags, and subcommands.

- `aguard run` starts the daemon; systemd normally runs this command.
- `aguard orphans tick` checks for processes left by ended Claude Code sessions. Run with `-dry-run` to inspect a pass.
- `aguard tmp tick`, `report`, and `send-test` check `/tmp`, summarize recorded usage, or verify message delivery.
- `aguard memory tick` samples session memory; `report` reviews the recorded peaks.
- `aguard restart ask -pid N` asks whether `cl` may restart a session; useful for inspecting one decision.
- `aguard waits tick` processes transcript wait and failure events; `report` summarizes them.
- `aguard compact tick` evaluates idle sessions for compaction or reset; `report` reviews decisions and outcomes.
- `aguard disk tick`, `retro`, `traces`, and `report` inspect disk growth and retained work traces, including historical estimates.
- `aguard said tick` classifies configured session messages; `report` summarizes classifications and usage.
- `aguard bgtask tick` checks for forgotten background commands.
- `aguard agentwaits tick` processes agent-created waits; `report` summarizes them.
- `aguard cpu tick` samples CPU pressure and runnable work on configured hosts.
- `aguard io report`, `now`, `series`, and `profiles` inspect I/O stall cases and service data; `record`, `dump`, and `at` capture or inspect manual samples; `import` imports older observer data.
- `aguard gui watch` samples GUI responsiveness for a bounded period; `report` reviews saved samples.
- `aguard remote tick` processes remote graft failure events.
- `aguard night tick` checks the configured overnight run and digest markers.
- `aguard report` summarizes daemon activity and observer errors.

Observer commands support `-dry-run` where shown by `aguard` usage. Reports accept observer-specific time-window and output flags.

The repository also includes [`cl`](cmd/cl/README.md), a Claude Code session launcher that asks aguard for restart and compaction decisions.

## Configuration

The daemon reads `~/.config/aguard/config.toml` and reloads it on each observer pass. The [example configuration](deploy/config.example.toml) documents all available fields and defaults. The main sections are:

- `[admins]`: maps working-directory prefixes to session names that receive notices.
- `[orphans]`: policy for processes left by ended sessions; defaults to `observe`.
- `[tmp]`: `/tmp` ownership summaries and shared-cache cleanup; defaults to `observe`.
- `[memory]`: records session memory peaks without enforcing a limit.
- `[restart]`: restart decision settings used by `cl`; defaults to `log`.
- `[agentwaits]`: collection and retention for waits agents create themselves.
- `[waits]`: transcript-derived wait, API failure, hook timeout, and compaction statistics.
- `[compact]`: idle-session compaction and reset decisions; defaults to `log`.
- `[disk]`: disk growth, free-space, and trace observation; record walks run after a notable free-space change, with a maximum interval; decisions are logged by default.
- `[bgtask]`: detection thresholds for forgotten background commands; alerts are enabled by default.
- `[cpu]`: CPU pressure sampling and alert routing; alerts are enabled by default.
- `[io]`: I/O and memory pressure stall sampling; alerts are enabled by default.
- `[gui]`: KDE Plasma and Chrome responsiveness sampling; defaults to logging.
- `[remote]`: detection of remote graft failures; alerts are enabled by default.
- `[codex]`: Codex weekly limit forecast; notices the lead before an early 100% and at 100%; alerts are enabled by default.
- `[night]`: overnight run and digest checks; alerts are enabled by default.
- `[said]`: transcript message classification; defaults to logging without writing the index.

Nested tables configure observer details: `[orphans.mute_until]` and `[[orphans.exclude]]` control orphan notices and exclusions; `[tmp.policy]`, `[tmp.pressure]`, `[tmp.escalation]`, and `[tmp.cache]` tune `/tmp` thresholds and cleanup; `[disk.owners]` maps paths to sessions, and `[[disk.traces]]` describes retained work traces. Unknown keys are errors. Most observers keep their state under `~/.local/state/aguard/`.

## Limitations

- Linux only; the daemon requires a systemd user manager and cgroup v2.
- Session discovery, transcript parsing, and session messages target Claude Code. `agentwaits` also reads Codex transcripts.
- `/tmp` ownership tracking depends on the separate root `tmpguard-track` service.
- I/O attribution accounts for filesystem writeback, including btrfs metadata and transaction work; disk usage uses FIEMAP where available.
- GUI sampling targets KDE Plasma (`kwin`, `plasmashell`) and Chrome processes, and collects matching user journal entries. Restart checks can read terminal content from tmux or KDE Konsole.
- Shared process and session support depends on [`github.com/aiseeq/akit`](go.mod).

## License

[MIT](LICENSE).
