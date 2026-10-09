# Nuntius v0.2.1 — specification as built

## Boundary

Nuntius is a Go, standard-library-only stdio MCP server for an owner-approved
judge on macOS or Linux. Linux CI tests the same process and file contracts. The module
is `github.com/ozgurcd/nuntius`. It has no network client, LLM or shell runner.
Version v0.2.1 is compiled in; publication is a separate release workflow.

Do not register it for coding agents. The MCP host owns approval. The server
cannot verify that a caller is the judge or force a host to display a click.
The owner must keep dispatch/reply/cancel approvals enabled in Desktop.

## Lanes

The preferred format is pools: one entry per claude/codex kind, size 0..8,
and one template (binary, workdir, extra_args, start_mode, continue_text).
Agents expand to claude-1..N and codex-1..N. Templates are validated even
at size zero. Both pools can be disabled. Legacy lanes still load; lanes
and pools cannot be mixed in the file.

config convert numbers each kind in original order (identuum -> claude-1)
and preserves stored session associations. Busy lanes, more than eight of
one kind, or differing templates refuse conversion. set claude-agent-pool N
and set codex-agent-pool N change size only, refuse removal of a busy agent,
and rewrite config atomically as 0600. A pool needs an owner template.
No MCP tool changes config. Servers reload it on checks and before dispatch.
The store control lock serializes these changes with starts and freeze.

The owner supplies `lanes`, `max_runtime` and `reports_dir` in lanes.json.
Each lane has name, kind (claude/codex), absolute binary, absolute workdir and
optional extra_args, start_mode and continue_text. Names are 1..64 ASCII letters,
digits, dots or hyphens and begin with a letter or digit. Names are unique.
There are 1..32 lanes. Binary must exist and be executable; workdir must exist.
The report directory is absolute and may be created later by the owner.

Config resolves from --config, NUNTIUS_CONFIG, then
$XDG_CONFIG_HOME/nuntius/lanes.json or ~/.config/nuntius/lanes.json, on macOS too.
The maximum config size is 64 KiB. Duration is required, positive and at most
seven days. Notifications default on and can be disabled in config.
Model selection and Claude effort/budget arguments are permitted.
Claude also permits --permission-mode with auto, acceptEdits, plan or manual.
Codex permits the valueless --skip-git-repo-check and --sandbox with read-only
or workspace-write. Value-taking flags accept split and equals forms.
All other permission, hook, sandbox, trust, cwd, output and session overrides
are refused, including bypassPermissions, dontAsk and danger-full-access.
No child environment values are injected; normal owner CLI settings apply.

## Runs

Each dispatch validates its lane, goal ID and 1..12000-byte prompt. Goal IDs
match `^[A-Z0-9][A-Z0-9.-]{1,63}$`. An OS file lock enforces one run per lane
across server instances. A recorded surviving PID also prevents lane reuse.
Only configured argv is executed, in its configured directory, with prompt stdin.

Claude argv: `-p --output-format stream-json --verbose`, then optional
`--resume ID`, then owner extra_args. Codex argv: `exec --json`, then
extra_args, then optional `resume ID`, then `-`. Exec-only options precede
resume. CLI help and the strict offline fake verify option scope, not a real run.

The data directory resolves from --store, NUNTIUS_DATA_DIR, then macOS
~/Library/Application Support/nuntius or Linux $XDG_DATA_HOME/nuntius
(~/.local/share/nuntius when unset). Other OS have no default and require
an explicit data directory. It holds runs, audit, notes, freeze, signals and
locks and belongs outside repositories. Run directories are 0700;
goal.txt, events.jsonl, stderr.log, meta.json and audit.jsonl are 0600.
Stdout/stderr go straight to files. Metadata is atomically replaced and includes
run ID, lane, goal ID, prompt SHA-256, start/end, exit, PID, process start time, state, session ID
and continuation links. Completed folders are published atomically. Run files
and report files refuse symlink leaves; report reads are rooted to reports_dir.
The append-only audit has time, tool, lane, goal ID, prompt hash and result.
Raw private storage is not redacted; all returned text is.

Normal completion is ended. Timeout metadata uses timed_out; status presents
ended with outcome timed_out. SIGINT escalates to SIGKILL after 30 seconds.
Cancel uses the same escalation. Runs start with setsid and direct file output.
Server shutdown detaches and leaves runs alive. Every signal requires a live
group leader with the recorded PID and kernel start time. macOS end notifications use fixed
AppleScript with data passed as argv; notification failure is ignored.
Notifications are skipped outside macOS and for an unknown exit code.
Every terminal outcome writes an empty private signals/<agent>.<goal_id>.ended
file through atomic exclusive creation; duplicate names coalesce. Recovery
also writes a signal. A normal completion's signal persistence failure is audited.

Restart reads persisted metadata. A dead run becomes ended with unknown exit.
Verified survivors regain supervision under an exclusive per-run supervisor lock.
Darwin uses the kernel's microsecond start time; Linux uses boot ID plus start
ticks. Mismatched identities are never signalled and become ended with unknown
exit. Legacy records with a live PID but no identity remain busy and unsignalable.
Never infer an exit code. While all servers are down timeout supervision is absent.

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
notification precede tools. The tools are:

- nuntius_status(): an object with frozen, freeze time/reason and lanes. Each lane
  has state idle/running/ended/ended_no_report, goal/run IDs, start, elapsed,
  last event time and age, exit, outcome and report existence.
- nuntius_dispatch(lane, goal_id, text): start when idle; return run_id.
- nuntius_reply(lane, text): resume the ended last run when its session ID exists.
- nuntius_tail(lane, lines=50): 1..200 physical lines of assistant text and tool
  names; no raw tool results. The last complete-event window is at most 1 MiB.
  A line is limited to 4096 bytes; returned text totals at most 60,000 bytes.
  Exception: an ended run without a report returns the final assistant message
  in one entry, capped at 8 KiB after redaction, without the line-count limit.
- nuntius_report(goal_id): a rooted regular report, at most 64 KiB after redaction.
- nuntius_cancel(lane): request SIGINT of a verified running process group.
- nuntius_freeze(reason): persist a state-dir marker and interrupt verified runs.
  Dispatch, reply, queued delivery and automatic continuation refuse while frozen.
- nuntius_unfreeze(): remove the marker. The host must ask the owner each time.
- nuntius_owner_notes(since): up to 50 redacted notes after an RFC3339 timestamp.
- nuntius_owner_notes_ack(ids): persist acknowledgements for 1..50 known IDs.
- nuntius_signals(): only a sorted JSON array of pending end-signal names.
- nuntius_signals_ack(names): remove 1..10000 named signals, idempotently;
  validate all names first and refuse traversal, directory and linked entries.

Notes and acknowledgements append to private notes.jsonl. Notes have at most
12000 bytes; reads refuse a file over 16 MiB. The judge owns the digest and
decides when to check notes. No settings editor or LLM is part of this program.

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

Commands: mcp, status [--json], agents [--json], paths, web [--bind 127.0.0.1:PORT], unfreeze,
watch LANE, config check, config convert, set KIND-agent-pool N, version and help. Global config
and store flags precede the command. Status and agents default to human views;
--json preserves their machine shapes. MCP output stays JSON. Status columns
show name/state/goal/elapsed/ETA/event age/activity/report; the header shows
freeze state and data directory. Idle agents become frozen in the human view
only. ETA needs at least three earlier completed same-kind/same-size runs;
goal size comes only from the first line. Median minus elapsed is shown as
approximate minutes left, or minutes over the median. Missing history gives -.
Activity uses fixed labels without command arguments. No colour escapes.
Paths prints resolved config/data and provenance without opening either.
An empty selected run store and nonempty default store produce a stderr warning
without moving data. The app needs explicit env entries to share shell overrides.
Watch prints changed tail
snapshots and ends when the run ends. Ctrl-C cancels the watch only.

`make verify` runs formatting, build, vet, Staticcheck, unit tests, race tests
and govulncheck. Unit tests also enforce explicit darwin/linux-only goos blocks
in the release config, refusing unknown OS and unsupported YAML forms.
Tests compile only the offline testagent fixture and use temp
directories. Required cases: config/bypass refusal, busy lanes, timeout kill,
restart recovery, redaction, goal ID and size bounds, sessionless reply refusal,
MCP round trip, and the three amended continuation behaviors. Additional cases
cover private modes, argv, cwd, audit, report links/bounds, CLI, multiline tails
and refusal to signal recovered unverified PIDs. V2 tests also cover killed-server
recovery, start-time mismatches, durable freeze/notes, pool conversion/resize,
strict Codex resume argv, and HTTP guards. All agents are offline fakes.

## Local dashboard

Only 127.0.0.1 binds are accepted. A new random credential is printed in the
start URL and required on every request. Host must be 127.0.0.1 or localhost
with the listener port; POST Origin must equal the local HTTP origin. Actions
accept only POST with the credential in a header. No CORS, external assets or
access logs. The browser removes the credential from its address bar.

Bounded redacted views show agent state/tails, the latest 50 runs' goal/report,
highlighted QUESTIONS/DECISIONS, audit, owner notes and OVERSIGHT.md next to
reports_dir (or an absolute owner-set oversight_path). DOM text nodes prevent
agent text from becoming active HTML. Raw stderr, tool results and session IDs
are excluded. Controls cancel, freeze, unfreeze, write notes and send messages.
One durable private message can queue per running run. Stop-and-send interrupts
the run and delivers after it ends; freeze suppresses queued delivery.

## Not included

No real-agent integration test, scheduler, public listener, automatic deletion,
app/agent-settings edits or LLM. Release publishing belongs to CI.
Store scans refuse over 10,000 entries; archive only ended records externally.
