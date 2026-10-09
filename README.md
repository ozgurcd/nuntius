# nuntius

Nuntius v0.2.2 is a small local Go MCP server for a judge using Claude Desktop.
It starts and follows configured Claude Code and Codex agent pools on macOS or Linux.
It contains no LLM and makes no network requests of its own. Agent programs may
use the network under their existing settings.

Register nuntius in Claude Desktop only. Do not register it in Claude Code or
Codex. Coding agents must not dispatch other agents. Configure the host to ask
the owner before every dispatch, reply, cancel and unfreeze. MCP annotations describe
side effects; they do not enforce a click or identify who called the server.

## Build and check

Use the Go version in `go.mod` (1.27.2), Staticcheck 2026.2.1 (0.8.1), and
govulncheck v1.8.0. CI gets Go and the checksum-verified Staticcheck build from
the pinned Lictor composite action used by Legattus. No Go library dependencies
are required. macOS and Linux are supported; Windows is not a target.

```sh
make verify
```

Install the published release through the owner's Homebrew tap. Never install
nuntius into `~/go/bin`.

```sh
brew install --cask ozgurcd/tap/nuntius
nuntius version
```

The tests compile an offline Go fake. They never execute the real agents,
load agent settings, or make network calls. The separate govulncheck gate reads
the public vulnerability database.

## Owner configuration

Create `~/.config/nuntius/lanes.json`. Use absolute executable and workspace
paths from your machine. The following paths are placeholders, not defaults:

```json
{
  "pools": {
    "claude":{"size":1,"template":{"workdir":"/absolute/workspace","binary":"/absolute/bin/claude","extra_args":[],"start_mode":"goal_then_continue","continue_text":"continue"}},
    "codex":{"size":2,"template":{"workdir":"/absolute/workspace","binary":"/absolute/bin/codex","extra_args":[],"start_mode":"as_is"}}
  },
  "max_runtime":"8h",
  "reports_dir":"/absolute/workspace/reports",
  "notifications":true
}
```

`notifications` is optional and defaults to true. Lane `start_mode` defaults
to `goal_then_continue` for Claude and `as_is` for Codex. `continue_text`
defaults to `continue`. Set Claude to `as_is` to disable automatic continuation.
Codex supports `as_is` only. The text and start mode cannot be supplied by MCP.

Each kind has one template and a size from 0 to 8. Agents are named
`claude-1` through `claude-N` and `codex-1` through `codex-N`. A disabled
pool retains its template. Templates are validated even when their size is zero.
No MCP tool changes pool sizes or templates. The CLI rewrites configuration
atomically with mode 0600; shrinking below a busy agent is refused.

Legacy `lanes` files still load. When all lanes are idle, convert them:

```sh
nuntius config convert
nuntius set claude-agent-pool 1
nuntius set codex-agent-pool 2
nuntius agents
nuntius agents --json
```

Conversion maps the Claude lane `identuum` to `claude-1`. Agents of each
kind are numbered in their original lane order. It preserves recorded sessions
under their converted names. Differing templates within one kind, more than
eight agents of one kind, or busy lanes refuse conversion without guessing
which settings to keep. Reconcile templates in the owner file first.
Existing servers reload configuration on status checks and before dispatch.

Unknown fields, duplicate lane names,
relative paths, non-executable binaries, invalid working directories, empty
lane sets, more than 32 lanes, and invalid durations are refused. `max_runtime`
must be positive and no longer than seven days. Configuration is limited to
64 KiB. No setting is read from agent configuration files by nuntius.

`extra_args` supports `--model`/`-m` for both kinds, and `--effort` and
`--max-budget-usd` for Claude. Claude also permits `--permission-mode` with
exactly `auto`, `acceptEdits`, `plan` or `manual`. Codex permits
`--skip-git-repo-check` without a value and `--sandbox` with exactly `read-only`
or `workspace-write`. Value-taking options support split and `--name=value`
forms. These options are agent-specific. Other options
are refused, including permission/sandbox bypasses, hook or settings overrides,
working-directory changes, session selection and output-format overrides.
This also closes indirect bypasses through Codex `-c` and Claude `--settings`.
The child inherits the server environment unchanged. The owner must start
Desktop with the intended normal environment and agent settings.

```sh
nuntius config check
nuntius status
nuntius status --json
nuntius paths
nuntius watch claude-1
```

`status` and `agents` print human views by default; add `--json` for machine
output. Status JSON retains the v0.2.0 shape, and MCP tools always return JSON.
The aligned status view shows agent, state, goal, elapsed, ETA, last event age,
last activity kind and report presence. Its header shows freeze state and the
data directory. Idle agents show `frozen` during a freeze; active and ended
agents keep their run state. No terminal colour escapes are emitted.

ETA uses the first line's `size S`, `size M` or `size L` and the median duration
of earlier finished runs of the same agent kind and size. Only runs that ended
before the current run started count. With fewer than three matches or unavailable
history it shows `-`; otherwise it shows `~12m left (n=5)` or
`over usual (+8m)`. It is an estimate, never a dispatch or waiting rule.
Activity shows fixed kinds such as `editing files`, `running tests` and
`git push`, never tool arguments or command text. Unknown kinds use a generic label.

Global `--config FILE` and `--store DIRECTORY` flags precede the command.
One private data directory holds `runs/`, `audit.jsonl`, `notes.jsonl`,
`freeze.json`, `signals/` and control locks. Resolution order is `--store`,
`NUNTIUS_DATA_DIR`, then the OS default:

- macOS: `~/Library/Application Support/nuntius`.
- Linux: `$XDG_DATA_HOME/nuntius`, or `~/.local/share/nuntius` when unset.
- Other OS: no default; set `NUNTIUS_DATA_DIR` or `--store` explicitly.

Config resolution is `--config`, `NUNTIUS_CONFIG`, then
`$XDG_CONFIG_HOME/nuntius/lanes.json`, or `~/.config/nuntius/lanes.json` when
unset, including on macOS. Environment values must be nonempty; data paths must
be absolute. `nuntius paths` prints both resolved paths and their source
(`flag`, `env`, `default`) without loading config or creating a store.
If the selected store has no runs while the OS default holds runs, CLI stderr
prints a warning. No files move automatically. macOS notifications are skipped
on other systems. Release checks reject every build OS except Darwin/Linux.

The data directory must be outside source repositories.
`watch` prints changed tail snapshots until the
lane ends; it is not a raw transcript. Ctrl-C stops watching.

## Claude Desktop entry

Add a `nuntius` entry inside the existing `mcpServers` object of Claude
Desktop's configuration. Resolve the executable path with `brew --prefix`;
the cask links `nuntius` into that prefix's `bin` directory.
Do not overwrite other entries. This repository does not edit app settings.

```json
"nuntius": {
  "command": "/absolute/homebrew/prefix/bin/nuntius",
  "args": ["mcp"],
  "env": {"NUNTIUS_DATA_DIR": "/absolute/private/nuntius"}
}
```

The Claude app starts the MCP server without your shell's environment. If you
set `NUNTIUS_DATA_DIR` in your shell, set the same variable in the app's
`nuntius` entry as above, or the CLI and server will read different stores.
Set `NUNTIUS_CONFIG` there too if you override the config file in your shell.
Use `nuntius paths` locally to check the selected paths.

Restart Desktop after saving. Set dispatch, reply, cancel and unfreeze to ask each time.
Ask the judge for status first, then approve one small read-only pilot goal.
Check the first session ID, resume behavior, report and audit locally without
copying raw agent logs into chat.

## Away from the Mac

Use the Claude mobile app to tell the judge to check status, answer questions,
freeze work, or send a reply. The Mac runs nuntius; mobile reaches it through
the judge and Claude Desktop, not through a public dashboard port.

Keep the Mac on power and awake. Enable Battery's “Prevent automatic sleeping
when the display is off” setting, or run `caffeinate -s`. Keep Claude Desktop
open so the judge can reach MCP. Runs survive a Desktop/server restart, but
timeout supervision resumes when a server opens the store again.

Tool approvals in Claude Desktop:

- **Ask**: `nuntius_dispatch`, `nuntius_reply`, `nuntius_cancel`, `nuntius_unfreeze`.
- **Allow**: `nuntius_status`, `nuntius_tail`, `nuntius_report`,
  `nuntius_owner_notes`, `nuntius_owner_notes_ack`, `nuntius_freeze`.
  `nuntius_signals` and `nuntius_signals_ack` read and acknowledge end signals.

Freeze is immediate: it persists a marker, interrupts verified running runs,
and refuses dispatch, reply and automatic continuation until unfreeze.
Unfreeze through the judge requires the owner's tap in the host. The local
dashboard and `nuntius unfreeze` also unfreeze. Status returns an object with
`frozen`, `freeze` (time and redacted reason) and `lanes`, including each
running lane's `last_event_age_seconds` to spot a hung run. Freeze still shows
when both pools are disabled. Owner notes are append-only in `notes.jsonl`;
the judge reads at most 50 after `since` (RFC3339) and acknowledges their IDs.

## Local oversight dashboard

```sh
nuntius web
```

Open the private URL printed by that invocation. It contains a new random
credential each start; do not share or log the URL. The page removes it from
the address bar after loading. Binding is restricted to `127.0.0.1`; use
`nuntius web --bind 127.0.0.1:8124` for another port. No LAN/remote bind or CORS
is enabled. Every request requires the credential, Host must match a loopback
hostname and port, and POST Origin must match the request's local origin.
Actions accept only POST and require the credential in a request header.

The dashboard adapts to laptop and phone-width windows and your system's light
or dark mode. The sticky top bar shows dispatch/freeze controls, running, idle
and ended counts, and refresh freshness on every tab. Agents opens by default;
Messages & notes, Digest, History and Audit have their own panels. Arrow keys
and Home/End navigate tabs. The URL hash selects the view when opening the
authenticated start URL. The tab bar scrolls within phone-width windows.
Compact agent cards show kind, state, goal, elapsed time, ETA, event age, activity and
report presence. Open a card's tail to read its bounded output. Cancel uses
an in-page confirmation before interrupting a running agent; Keep running or
the timeout restores the initial button. Message opens that agent's composer.

The other tabs show messages and notes, the judge's `OVERSIGHT.md` digest next
to the reports directory (or optional absolute `oversight_path` in config),
the latest 50 runs and the bounded audit log. Filter history by goal ID or lane,
then expand a row for highlighted QUESTIONS/DECISIONS, goal and report.
Times are relative with absolute dates on hover.
History rows stack on phones without splitting badge, lane or duration words.
Tab counts show running agents, unread owner notes and runs with open questions.

Polling runs every two seconds; hidden panels update when shown. Stable cards and rows keep focus and expansion;
reading an open tail, report or audit log pauses its content updates. Sections
pause briefly after interaction and while focused or selected. A visible banner
reports refresh failures while retaining the last data. All assets are embedded,
offline and authenticated; CSP permits only same-origin scripts/styles, with no
inline code. All report and note text is inserted as text. Output stays redacted;
raw tool results, session IDs and stderr are not dashboard data.

Send a message to an ended session, queue one message for delivery after a
running run ends, or choose “Stop and send”. The pending message is private and
durable. Freeze suppresses delivery. Duplicate queued messages are refused.
Cancel, Freeze, Unfreeze and owner-note entry are local owner actions.

## Measured CLI interface

Measured using help only on 2026-10-09: Claude Code 2.1.295 and Codex CLI
0.159.2. No real agent run was used to verify event shapes.

- Claude start: `claude -p --output-format stream-json --verbose [extra_args]`.
- Claude resume: the same flags plus `--resume SESSION_ID` before extra args.
  Resume is a top-level option, not a separate subcommand.
- Codex start: `codex exec --json [extra_args] -`.
- Codex resume: `codex exec --json [extra_args] resume SESSION_ID -`.
  `--sandbox` is an `exec` option, not a `resume` option; flags precede `resume`.
  Verified again from `codex exec resume --help`; the fake rejects the old order.

Each prompt is sent only on stdin. There is no shell and no injected environment.
Working directory is the lane's configured directory. Help confirms the argv
forms; it does not establish that noninteractive permission prompts work as
expected in the owner's app. A denied permission is never answered by nuntius.

The parser accepts top-level `session_id`, `thread_id`, and nested
`session.id`/`thread.id`. Unknown event fields are ignored. A missing or unsafe
session ID disables reply; it is not guessed. Session detection reads the first
1 MiB of complete events. These assumptions need the owner's pilot.

For a successful first Claude run in `goal_then_continue` mode, nuntius scans
the complete event stream for work. An assistant acknowledgement with no tool
calls starts exactly one resume. Tool calls, a missing ID, malformed or oversized
events, unfamiliar event types, cancellation, timeout or a nonzero exit suppress
the automatic resume. Each run's metadata links the other run. The lane lock
stays held between them. A manual reply never triggers another automatic reply.

## MCP interface and storage

The tools are `nuntius_status`, `nuntius_dispatch`, `nuntius_reply`,
`nuntius_tail`, `nuntius_report`, `nuntius_cancel`, `nuntius_freeze`,
`nuntius_unfreeze`, `nuntius_owner_notes`, `nuntius_owner_notes_ack`,
`nuntius_signals` and `nuntius_signals_ack`. See `PROJECT_SPEC.md` for
arguments, limits and failure behavior. The transport uses newline-delimited
JSON-RPC over stdio and MCP 2025-11-25. Stdout carries protocol messages only.
References: [stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
and [MCP tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools).

An ended run without a report has status `ended_no_report`. Its tail returns
the final assistant message as one text entry, up to 8 KiB after redaction,
regardless of the requested line count. `asked_interactively` flags a known
AskUserQuestion or request_user_input attempt in the last 1 MiB of events,
including refused attempts. The judge can answer with `nuntius_reply`.
Unknown question event shapes need the owner pilot; no tool arguments or
tool results are returned. A report from an older run with the same goal ID
still counts as present; use distinct goal IDs for distinct tasks.

Run folders are private (0700), and files are private (0600). They include the
full prompt and raw agent stdout/stderr, which can contain sensitive material.
Do not share them. MCP and owner CLI output redact the specified secret classes;
redaction is not a claim that arbitrary prose can never contain private data.
The audit log records tool, lane, goal, prompt hash and result, not prompt text.
No automatic history deletion occurs. Archive ended runs before reaching the
10,000-entry limit. Incomplete `.pending-*` folders are not exposed as runs.

Every run end, including failure, cancellation, timeout or restart recovery,
atomically creates an empty private `signals/<agent>.<goal_id>.ended` file.
`nuntius_signals()` returns only a sorted JSON list of pending names.
`nuntius_signals_ack(names)` removes the named signals; paths, traversal and
linked files are refused. Duplicate agent/goal signals coalesce until acknowledged;
use distinct goal IDs when each run needs a separate signal. Acknowledgements
are idempotent. A failed signal write is recorded in the audit log.

Timeout sends SIGINT to the run's process group, then SIGKILL after 30 seconds.
Cancel follows the same escalation. Each run starts in its own session with
stdout/stderr directed straight to private files. Shutdown detaches supervision
and leaves the agents running. On restart, dead runs become ended with unknown
exit; verified surviving runs regain timeout/cancel supervision. The recorded
PID and kernel process start time must match before every signal. Darwin uses
microsecond kernel start times, Linux boot ID plus process start ticks. A run
with a mismatched identity is ended with unknown exit, never signalled. Legacy
v1 records with a live PID but no start identity remain busy and cannot be
signalled safely. Inspect those locally; identity and exit codes are never guessed.

Malformed configuration or an unavailable store leaves MCP in a non-serving
state: initialize/list still work; tool calls report the fault. Run `config
check`, correct the owner configuration, then restart the server. No agent
settings editor, public listener, automatic deletion or LLM is included.
