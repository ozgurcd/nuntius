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

Restart Desktop after saving. Set dispatch, reply, cancel, unfreeze, web ensure,
lane pause and lane resume to Ask each time. Both lane controls need deliberate
owner approval for every invocation, including repeated requests.
Ask the judge for status first, then approve one small read-only pilot goal.
Check the first session ID, resume behavior, report and audit locally without
copying raw agent logs into chat.

## MCP interface and storage

The tools are `nuntius_status`, `nuntius_history`, `nuntius_dispatch`, `nuntius_reply`,
`nuntius_tail`, `nuntius_report`, `nuntius_cancel`, `nuntius_freeze`,
`nuntius_unfreeze`, `nuntius_owner_notes`, `nuntius_owner_notes_ack`,
`nuntius_signals`, `nuntius_signals_ack`, `nuntius_web_ensure`,
`nuntius_lane_pause` and `nuntius_lane_resume`. See [PROJECT_SPEC.md](../PROJECT_SPEC.md) for
arguments, limits and failure behavior. The transport uses newline-delimited
JSON-RPC over stdio and MCP 2025-11-25. Stdout carries protocol messages only.
References: [stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)
and [MCP tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools).

### Lane pause and resume

Read `nuntius_status({})` first. Its versioned `lane_control` snapshot lists
configured lanes and retained names, sorted by name, with the current store
revision, holds and reasons that prevent dispatch. Run state, failure, report
presence and attention badges remain separate. There is no extra list tool.

`nuntius_lane_pause({"lane":"codex","expected_revision":0})` saves a hold.
Use the actual revision from status. New starts stop immediately; the current
run may finish with its normal timeout and supervision. Optional
`"after_current":true` has the same immediate admission boundary. Pause does
not cancel, signal or extend the current run.

`nuntius_lane_resume({"lane":"codex","expected_revision":1})` clears only
the hold. This operation starts no work and does not replay held messages.
Normal recovery or a later approved dispatch can act separately; other
freeze, recovery and message-review blockers still apply. A retained removed
name can be resumed without recreating its configuration.

Arguments are closed objects of at most 4 KiB. Lane names use the existing
name grammar. `expected_revision` is required, from 0 through 9007199254740991,
and must use integer JSON syntax. Nulls, duplicates, strings in place of numbers
or booleans, fractions and unknown fields are refused. Only pause accepts
`after_current`, a strict boolean with default false. Caller roles, sources,
paths and host settings are not accepted.

New controls return `structuredContent` plus one text entry containing the
identical JSON. The object has `operation`, `lane`, `code`, `changed`,
`revision`, `lane_control` and a plain `explanation`. A successful save returns
`committed`; a fresh same-state request returns `unchanged` without advancing
the revision. A stale request returns `revision_conflict`, even if its desired
state matches. A successful snapshot is captured under the mutation's control
lock. A refusal can include a later safe observation. Unknown revisions
and unreadable snapshots are null, never guessed. Read status before an approved
retry; never blindly repeat an uncertain save or audit result.

Paused dispatch/reply refusals also use structured content and identical text
JSON: `code:"lane_paused"`, the lane, `paused:true`, `retryable:false`, safe
`blockers` (including `frozen` when applicable) and a plain explanation.
Application failures set `isError:true` in the tool result. They are not numeric
JSON-RPC protocol errors. Dispatch/reply errors after durable publication return
`code:"launch_uncertain"`, the short run ID and `retryable:false`. Cancel errors
after durable StopAt return `code:"stop_recorded"`; escalation still follows.
Effects without confirmed publication return `code:"uncertain"`.
After any such result, or no response at all (timeout, EOF, cancelled or killed
client), reconcile status/history by lane, goal and time window before sending
another dispatch/reply. Only an explicit pre-effect refused/locked result is
retry-safe. A reserved held receipt requires owner review; it is never cleared
silently. There is no request-id deduplication in this hotfix.
The manager owns control intent/outcome audit; the transport adds no duplicate
generic outcome for manager-audited mutations. Audit delivery after repair may
repeat a correlated outcome; exactly-once delivery is not claimed.

Both tools declare readOnlyHint=false, idempotentHint=true, openWorldHint=false
and conservative destructiveHint=true. These describe effects, not identity or
approval enforcement. Ask is the host policy. A separate same-user stdio client
with store access can invoke them. Register only in the owner's judge host;
do not register coding agents or change their permissions, sandboxes or hooks.
This server never writes MCP host settings. See [security notes](security.md).

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
An isolated lane-control fault instead blocks new starts and marks controls
unavailable. Status diagnostics, tails, reports, notes and independent safety
tools remain usable when their own state is healthy. This exception does not
bypass configuration, run-store or other startup faults.
