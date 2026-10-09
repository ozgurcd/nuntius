# nuntius

Nuntius v0.2.0 is a small local Go MCP server for a judge using Claude Desktop.
It starts and follows configured Claude Code and Codex agent pools on the owner's Mac.
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
nuntius watch claude-1
```

Global `--config FILE` and `--store DIRECTORY` flags precede the command.
The default store is `~/Library/Application Support/nuntius`. It must be
outside source repositories. `watch` prints changed tail snapshots until the
lane ends; it is not a raw transcript. Ctrl-C stops watching.

## Claude Desktop entry

Add a `nuntius` entry inside the existing `mcpServers` object of Claude
Desktop's configuration. Resolve the executable path with `brew --prefix`;
the cask links `nuntius` into that prefix's `bin` directory.
Do not overwrite other entries. This repository does not edit app settings.

```json
"nuntius": {
  "command": "/absolute/homebrew/prefix/bin/nuntius",
  "args": ["mcp"]
}
```

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

The dashboard polls agent states and readable tails. It shows the latest 50
runs with their goal, report and highlighted QUESTIONS/DECISIONS, the bounded
audit log, owner notes, and the judge's `OVERSIGHT.md` digest next to the reports
directory (or optional absolute `oversight_path` in config). It uses inline
assets and inserts all report and note text safely as text. Output is redacted;
raw tool results, session IDs and stderr are not dashboard data.

Send a message to an ended session, queue one message for delivery after a
running run ends, or choose “Stop and send”. The pending message is private and
durable. Freeze suppresses delivery. Duplicate queued messages are refused.
Cancel, FREEZE ALL, UNFREEZE and owner-note entry are local owner actions.

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
`nuntius_unfreeze`, `nuntius_owner_notes` and `nuntius_owner_notes_ack`. See `PROJECT_SPEC.md` for
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
