# nuntius

Nuntius v0.1.1 is a small local Go MCP server for a judge using Claude Desktop.
It starts and follows configured Claude Code and Codex lanes on the owner's Mac.
It contains no LLM and makes no network requests of its own. Agent programs may
use the network under their existing settings.

Register nuntius in Claude Desktop only. Do not register it in Claude Code or
Codex. Coding agents must not dispatch other agents. Configure the host to ask
the owner before every dispatch, reply and cancel. MCP annotations describe
side effects; they do not enforce a click or identify who called the server.

## Build and check

Use the Go version in `go.mod` (1.27.2), Staticcheck 2026.2.1 (0.8.1), and
govulncheck v1.8.0. CI gets Go and the checksum-verified Staticcheck build from
the pinned Lictor composite action used by Legattus. No Go library dependencies
are required. macOS and Linux are supported; Windows is not a target.

```sh
make verify
make install
"$(go env GOPATH)/bin/nuntius" version
```

The tests compile an offline Go fake. They never execute the real agents,
load agent settings, or make network calls. The separate govulncheck gate reads
the public vulnerability database.

## Owner configuration

Create `~/.config/nuntius/lanes.json`. Use absolute executable and workspace
paths from your machine. The following paths are placeholders, not defaults:

```json
{
  "lanes": [
    {"name":"identuum","kind":"claude","workdir":"/absolute/workspace","binary":"/absolute/bin/claude","extra_args":[],"start_mode":"goal_then_continue","continue_text":"continue"},
    {"name":"codex-1","kind":"codex","workdir":"/absolute/workspace","binary":"/absolute/bin/codex","extra_args":[],"start_mode":"as_is"},
    {"name":"codex-2","kind":"codex","workdir":"/absolute/workspace","binary":"/absolute/bin/codex","extra_args":[],"start_mode":"as_is"}
  ],
  "max_runtime":"8h",
  "reports_dir":"/absolute/workspace/reports",
  "notifications":true
}
```

`notifications` is optional and defaults to true. Lane `start_mode` defaults
to `goal_then_continue` for Claude and `as_is` for Codex. `continue_text`
defaults to `continue`. Set Claude to `as_is` to disable automatic continuation.
Codex supports `as_is` only. The text and start mode cannot be supplied by MCP.

The config is read once at startup. Unknown fields, duplicate lane names,
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
nuntius watch identuum
```

Global `--config FILE` and `--store DIRECTORY` flags precede the command.
The default store is `~/Library/Application Support/nuntius`. It must be
outside source repositories. `watch` prints changed tail snapshots until the
lane ends; it is not a raw transcript. Ctrl-C stops watching.

## Claude Desktop entry

Add a `nuntius` entry inside the existing `mcpServers` object of Claude
Desktop's configuration. Resolve the executable path with `go env GOPATH`.
Do not overwrite other entries. This repository does not edit app settings.

```json
"nuntius": {
  "command": "/absolute/go/bin/nuntius",
  "args": ["mcp"]
}
```

Restart Desktop after saving. Set dispatch, reply and cancel to ask each time.
Ask the judge for status first, then approve one small read-only pilot goal.
Check the first session ID, resume behavior, report and audit locally without
copying raw agent logs into chat. Keep Desktop open while v1 runs work.

## Measured CLI interface

Measured using help only on 2026-10-09: Claude Code 2.1.295 and Codex CLI
0.159.2. No real agent run was used to verify event shapes.

- Claude start: `claude -p --output-format stream-json --verbose [extra_args]`.
- Claude resume: the same flags plus `--resume SESSION_ID` before extra args.
  Resume is a top-level option, not a separate subcommand.
- Codex start: `codex exec --json [extra_args] -`.
- Codex resume: `codex exec resume SESSION_ID --json [extra_args] -`.

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

The six tools are `nuntius_status`, `nuntius_dispatch`, `nuntius_reply`,
`nuntius_tail`, `nuntius_report` and `nuntius_cancel`. See `PROJECT_SPEC.md` for
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
Cancel follows the same escalation. Graceful MCP shutdown cancels owned runs.
On restart, dead runs become ended with unknown exit. A surviving process keeps
its lane busy. V1 does not signal a process owned by a previous or other server:
PID/start-time verification and detached supervision belong to v2. Consequently
a hard server crash can leave a live process without v1 timeout supervision.
Do not delete live records or start a second server to take over such a run.

Malformed configuration or an unavailable store leaves MCP in a non-serving
state: initialize/list still work; tool calls report the fault. Run `config
check`, correct the owner configuration, then restart the server. No dashboard,
freeze controls, agent settings editor, HTTP listener or release automation is
included in v1.
