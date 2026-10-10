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

Restart Desktop after saving. Set dispatch, reply, cancel, unfreeze and web ensure to ask each time.
Ask the judge for status first, then approve one small read-only pilot goal.
Check the first session ID, resume behavior, report and audit locally without
copying raw agent logs into chat.

## MCP interface and storage

The tools are `nuntius_status`, `nuntius_history`, `nuntius_dispatch`, `nuntius_reply`,
`nuntius_tail`, `nuntius_report`, `nuntius_cancel`, `nuntius_freeze`,
`nuntius_unfreeze`, `nuntius_owner_notes`, `nuntius_owner_notes_ack`,
`nuntius_signals`, `nuntius_signals_ack` and `nuntius_web_ensure`. See [PROJECT_SPEC.md](../PROJECT_SPEC.md) for
arguments, limits and failure behavior. The transport uses newline-delimited
JSON-RPC over stdio and MCP 2025-11-25. Stdout carries protocol messages only.
References: [stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
and [MCP tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools).

`nuntius_web_ensure({})` is judge-only. It accepts only an empty
object (at most 4096 bytes), with no caller-selected path, host, port, recipient,
PID or command. It is state-changing and idempotent; host approval remains Ask
for every invocation. Refusals return stable state/reason codes. An unavailable
owner MCP cannot be replaced by dispatching a privileged coding task.
See [dashboard recovery](dashboard.md#judge-only-dashboard-recovery) for results
and [security notes](security.md) for boundaries.

An ended run without a report has status `ended_no_report`. Its tail returns
the final assistant message as one text entry, up to 8 KiB after redaction,
regardless of the requested line count. `asked_interactively` flags a known
AskUserQuestion or request_user_input attempt in the last 1 MiB of events,
including refused attempts. The judge can answer with `nuntius_reply`.
Unknown question event shapes need the owner pilot; no tool arguments or
tool results are returned. Report presence and stuck detection count only a
regular report modified at or after the current run started. Older reports do
not mark a redispatched goal complete or stuck. A report delivered late is
accepted until a newer run of the same goal owns that report timestamp.

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

Stuck detection creates the same private empty signal form as
`signals/<agent>.<goal_id>.stuck`. A per-run persisted flag prevents delivery
again after acknowledgement or restart, even if the run remains stuck.
Repeated lane/goal names coalesce while pending. Listing and acknowledgements
accept `.ended`, `.stuck`, `.interrupted` and `.resumed`, with the same bounds
and path protections. Recovery delivery flags survive acknowledgements.

Timeout sends SIGINT to the run's process group, then SIGKILL after 30 seconds.
Cancel follows the same escalation. Each run starts in its own session with
stdin/stdout/stderr directed straight to private files. Short-lived launch stages
exit before the agent executes under the OS parent. Shutdown leaves agents running.
Verified surviving runs regain timeout/cancel checks inside a live nuntius process.
Dead runs get unknown exit codes and file-derived outcomes, followed by the
recovery policy above when enabled. The recorded
PID and kernel process start time must match before every signal. Darwin uses
microsecond kernel start times, Linux boot ID plus process start ticks. A run
with a mismatched identity is ended with unknown exit, never signalled. Legacy
v1 records with a live PID but no start identity remain busy and cannot be
signalled safely. Inspect those locally; identity and exit codes are never guessed.

Malformed configuration or an unavailable store leaves MCP in a non-serving
state: initialize/list still work; tool calls report the fault. Run `config
check`, correct the owner configuration, then restart the server. No agent
settings editor, public listener, automatic deletion or LLM is included.
