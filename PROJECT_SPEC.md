# Nuntius v0.1.0 — specification as built

## Boundary

Nuntius is a Go, standard-library-only stdio MCP server for an owner-approved
judge on macOS. Linux CI tests the same process and file contracts. The module
is `github.com/ozgurcd/nuntius`. It has no network client, LLM or shell runner.
Version v0.1.0 is compiled in; no release tag is implied.

Do not register it for coding agents. The MCP host owns approval. The server
cannot verify that a caller is the judge or force a host to display a click.
The owner must keep dispatch/reply/cancel approvals enabled in Desktop.

## Lanes

The owner supplies `lanes`, `max_runtime` and `reports_dir` in lanes.json.
Each lane has name, kind (claude/codex), absolute binary, absolute workdir and
optional extra_args, start_mode and continue_text. Names are 1..64 ASCII letters,
digits, dots or hyphens and begin with a letter or digit. Names are unique.
There are 1..32 lanes. Binary must exist and be executable; workdir must exist.
The report directory is absolute and may be created later by the owner.

Config defaults to `~/.config/nuntius/lanes.json`; only a CLI flag changes it.
The maximum config size is 64 KiB. Duration is required, positive and at most
seven days. Notifications default on and can be disabled in config.
Only model selection and Claude effort/budget arguments are permitted.
Permission, hook, sandbox, trust, cwd, output and session overrides are refused.
No child environment values are injected; normal owner CLI settings apply.

## Runs

Each dispatch validates its lane, goal ID and 1..12000-byte prompt. Goal IDs
match `^[A-Z0-9][A-Z0-9.-]{1,63}$`. An OS file lock enforces one run per lane
across server instances. A recorded surviving PID also prevents lane reuse.
Only configured argv is executed, in its configured directory, with prompt stdin.

Claude argv: `-p --output-format stream-json --verbose`, then optional
`--resume ID`, then owner extra_args. Codex argv: `exec [resume ID] --json`,
then extra_args, then `-`. CLI help, not a real agent run, verified these forms.

Store defaults to `~/Library/Application Support/nuntius`, with CLI-only
`--store` override. It belongs outside repositories. Run directories are 0700;
goal.txt, events.jsonl, stderr.log, meta.json and audit.jsonl are 0600.
Stdout/stderr go straight to files. Metadata is atomically replaced and includes
run ID, lane, goal ID, prompt SHA-256, start/end, exit, PID, state, session ID
and continuation links. Completed folders are published atomically. Run files
and report files refuse symlink leaves; report reads are rooted to reports_dir.
The append-only audit has time, tool, lane, goal ID, prompt hash and result.
Raw private storage is not redacted; all returned text is.

Normal completion is ended. Timeout metadata uses timed_out; status presents
ended with outcome timed_out. SIGINT escalates to SIGKILL after 30 seconds.
Cancel and graceful server shutdown use the same escalation. Only owned
process groups can be signalled by v1. macOS end notifications use fixed
AppleScript with data passed as argv; notification failure is ignored.

Restart reads persisted metadata. A dead run becomes ended with unknown exit;
an observed surviving process stays busy. V1 cannot safely adopt or cancel that
process, nor guarantee its timeout after a hard server crash. Detached takeover
with process start-time identity is queued for v2. Never infer an exit code.

## Goal start amendment

Claude defaults to goal_then_continue. Codex defaults to and requires as_is.
An owner can select as_is for Claude. continue_text defaults to continue and
is at most 12000 bytes. These are config fields, never tool arguments.
One automatic same-session resume follows a successful initial Claude
acknowledgement only when a complete, recognized event stream shows no work.
Tool calls, failed/cancelled/timed-out runs, missing IDs or uncertain parsing
disable it. Parent and continuation metadata each contain both run IDs.
The lane remains locked throughout the transition. Resumes never auto-resume.

## MCP tools

Protocol 2025-11-25 over newline-delimited JSON-RPC. Initialize and the initialized
notification precede tools. Exactly six tools:

- nuntius_status(): all lanes, state idle/running/ended/ended_no_report, goal/run IDs, start,
  elapsed, last event time, exit, outcome and report existence.
- nuntius_dispatch(lane, goal_id, text): start when idle; return run_id.
- nuntius_reply(lane, text): resume the ended last run when its session ID exists.
- nuntius_tail(lane, lines=50): 1..200 physical lines of assistant text and tool
  names; no raw tool results. The last complete-event window is at most 1 MiB.
  A line is limited to 4096 bytes; returned text totals at most 60,000 bytes.
  Exception: an ended run without a report returns the final assistant message
  in one entry, capped at 8 KiB after redaction, without the line-count limit.
- nuntius_report(goal_id): a rooted regular report, at most 64 KiB after redaction.
- nuntius_cancel(lane): request SIGINT of a run owned by this server.

Unexpected tool argument keys are refused. Error messages do not echo inputs.
Input protocol lines are limited to 128 KiB. Unknown methods receive JSON-RPC
errors; tool failures use isError. Every tool describes output as untrusted data.
Initialization advertises session parser and permission limits. Configuration
or store startup faults leave a non-serving MCP transport, not a runnable lane.

Status flags asked_interactively for known AskUserQuestion/request_user_input
tool attempts in the last 1 MiB of events, including refusals. It does not claim
to detect unknown event shapes or an attempt outside that window. Report
existence is by goal ID, including an older report with the same ID.

Returned strings redact GitHub token prefixes, sk-/sk-ant- API keys, xox Slack
tokens, JWT-shaped strings, PEM private-key blocks (including incomplete blocks),
and lines containing password=/secret=/token= assignments, ignoring case and
spaces around the equals sign. Redaction precedes truncation and line slicing.

Session IDs are recognized as top-level session_id/thread_id or nested
session.id/thread.id in the first 1 MiB of complete events. IDs must be 1..128
safe ASCII identifier characters. Unknown shapes disable resume. Real event
shapes and Desktop permission interaction remain owner-pilot questions.

## CLI and validation

Commands: mcp, status, watch LANE, config check, version and help. Global config
and store flags precede the command. Status is JSON; watch prints changed tail
snapshots and ends when the run ends. Ctrl-C cancels the watch only.

`make verify` runs formatting, build, vet, Staticcheck, unit tests, race tests
and govulncheck. Tests compile only the offline testagent fixture and use temp
directories. Required cases: config/bypass refusal, busy lanes, timeout kill,
restart recovery, redaction, goal ID and size bounds, sessionless reply refusal,
MCP round trip, and the three amended continuation behaviors. Additional cases
cover private modes, argv, cwd, audit, report links/bounds, CLI, multiline tails
and refusal to signal recovered unverified PIDs.

## Not included

No real-agent integration test, queue beyond lane exclusion, dashboard, freeze,
owner notes, HTTP listener, automatic deletion, app-settings edits or release.
Store scans refuse over 10,000 entries; archive only ended records externally.
