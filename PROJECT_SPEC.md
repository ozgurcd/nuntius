# Nuntius v0.3.0 — specification as built

## Boundary

Nuntius is a Go stdio MCP server for an owner-approved
judge on macOS or Linux. Linux CI tests the same process and file contracts. The module
is `github.com/ozgurcd/nuntius`. It has no network client, LLM or shell runner.
Version v0.3.0 is compiled in; publication is a separate release workflow.
The optional menu-bar helper lives in its own module under menubar/. It uses
fyne.io/systray v1.12.0 (Cocoa/cgo on macOS, D-Bus StatusNotifierItem on Linux),
and is not imported by the pure-Go main binary. Background start launches it
when installed in a GUI session; --no-menubar opts out. Stop closes it.
Explicit menubar --at-login on|off writes/removes a helper-only LaunchAgent
for the next macOS login and never loads a job. See docs/menubar-design.md.
Terminal QR encoding uses rsc.io/qr v0.2.0, with no transitive modules.

Do not register it for coding agents. The MCP host owns approval. The server
cannot verify that a caller is the judge or force a host to display a click.
The owner must keep dispatch/reply/cancel approvals enabled in Desktop.

## Lanes

The preferred format is pools: one entry per claude/codex/opencode kind, size 0..8,
and one template (binary, workdir, extra_args, model, start_mode, continue_text).
Agents expand to claude-1..N, codex-1..N and opencode-1..N. Templates are validated even
at size zero. Pools can be disabled. Legacy lanes still load; lanes
and pools cannot be mixed in the file.

config convert numbers each kind in original order (identuum -> claude-1)
and preserves stored session associations. Busy lanes, more than eight of
one kind, or differing templates refuse conversion. set claude-agent-pool N
set codex-agent-pool N and set opencode-agent-pool N change size only, refuse removal of a busy agent,
and rewrite config atomically as 0600. A pool needs an owner template.
No MCP tool changes config. Servers reload it on checks and before dispatch.
The store control lock serializes these changes with starts and freeze.

The owner supplies `lanes`, `max_runtime` and `reports_dir` in lanes.json.
Each lane has name, kind (claude/codex/opencode), absolute binary, absolute workdir and
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
No agent settings are injected; normal owner CLI settings apply. The configured
workdir uses exec.Command's standard PWD behavior through the launch stages.
auto_resume defaults to true; false disables automatic crash-recovery launches.

OpenCode requires a printable 1..128-byte provider/model in the model field.
Its start_mode is as_is only. Only --agent and --variant are permitted extra
arguments; --auto, --pure, interactive, remote/attachment, model and session/output
overrides are refused. Nuntius reads no agent settings and injects no permissions.
Model choice and the guard/permission installation remain owner actions; the
Identuum pointer is wiki/tools/opencode/README.md, owned by OPENCODE-GUARD-1.

## Runs

Each dispatch validates its lane, goal ID and 1..12000-byte prompt. Goal IDs
match `^[A-Z0-9][A-Z0-9.-]{1,63}$`. An OS file lock enforces one run per lane
across server instances. A recorded surviving PID also prevents lane reuse.
Only configured argv is executed, in its configured directory, with prompt stdin.

Claude argv: `-p --output-format stream-json --verbose`, then optional
`--resume ID`, then owner extra_args. Codex argv: `exec --json`, then
extra_args, then optional `resume ID`, then `-`. Exec-only options precede
resume. CLI help and the strict offline fake verify option scope, not a real run.

OpenCode argv: run --format json --model MODEL, then extra_args and optional
--session ID. The unchanged file-backed stdin carries the full brief, including
12,000 bytes and literal quotes/newlines. No model is called by tests. Installed
CLI help was verified at 1.18.35; upstream run.ts defines piped message handling
and JSON fixtures. Real model event capture is unverified. Native sessionID is
captured safely; millisecond timestamp, part.text, tool/state/input, step_start,
step_finish and error are normalized through the shared action summarizer.
Tool results never enter returned summaries. Unknown events retain type and
first eligible string. Native MCP IDs server_tool display as server.tool with
the first underscore as separator; underscore-bearing server names are ambiguous.
Upstream tool events arrive at completion/error, so no running duration is claimed
without a start event. Recover/resume/cancel/freeze/timeout share existing paths.
Status/agents expose kind and effective model/effort. Claude/Codex model comes
from --model/-m (split or equals); Claude effort comes from --effort. Codex
effort comes only from structured startup events when present, otherwise
default; agent settings are never read. OpenCode keeps its template model.
Runs persist launch values for history, MCP and web data; old records show
default rather than borrowing current configuration.

The data directory resolves from --store, NUNTIUS_DATA_DIR, then macOS
~/Library/Application Support/nuntius or Linux $XDG_DATA_HOME/nuntius
(~/.local/share/nuntius when unset). Other OS have no default and require
an explicit data directory. It holds runs, audit, notes, freeze, signals and
locks and belongs outside repositories. Run directories are 0700;
goal.txt, events.jsonl, stderr.log, meta.json and audit.jsonl are 0600.
Stdout/stderr go straight to files. Metadata is atomically replaced and includes
run ID, lane, goal ID, original brief, prompt SHA-256, start/end, exit, PID,
process start time, state, session ID, recovery count and continuation links.
An integrity hash detects damaged records. Files use temp+fsync+rename+directory
fsync. Run folders are published and directory-synced before agent launch. Run files
and report files refuse symlink leaves; report reads are rooted to reports_dir.
The append-only audit has time, tool, lane, goal ID, prompt hash and result.
Raw private storage is not redacted; all returned text is.

Normal completion is ended. Timeout metadata uses timed_out; status keeps the
legacy ended/ended_no_report state and adds display_state timed_out. Cancelled
runs also retain their legacy state and add display_state cancelled.
SIGINT escalates to SIGKILL after 30 seconds.
Cancel uses the same escalation. Linux retains the short double-fork bootstrap,
new session, OS parent and exec used in v0.2.3. Darwin bootstraps a transient
`gui/<uid>/nuntius.run.<run_id>` job, then kickstarts it once. Its 0600 plist is
in the private run directory only. RunAtLoad and KeepAlive are false;
LaunchOnlyOnce is true. Umask inherits the dispatching process's startup mask,
with fallback 022; private file/directory modes remain 0600/0700.
An exec-only stage records PID/kernel start and becomes
the agent with unchanged argv/cwd/environment and file-backed standard streams.
Inherited environment crosses a 0600 FIFO during bounded startup only, never
a disk snapshot or a launchctl diagnostic. There is no runtime pipe helper.
The live manager bootouts the job after end/cancel/timeout and recovery retries
stale cleanup. Finished cleanup is persisted, avoiding repeated launchctl calls.
Only intact recorded run IDs supply labels; job print must report no running
process or a PID with the exact recorded start identity before bootout.
Pending job PIDs remain busy and unsignalable until bootstrap supplies a start
identity. A launchctl failure safely removes any partial job and uses detached
start, adding a fixed launchd_fallback event and launch_fallback status flag.
An unverifiable partial job prevents duplicate fallback; the goal remains recorded.
Failures before kickstart still dispatch through detached start when launchctl
cannot be queried; the inactive job label remains recorded for cleanup retries.
All standard descriptors are files; there is no controlling terminal. No
bootstrap, wrapper, watchdog, daemon, service or login item remains while the
agent runs. Server shutdown leaves runs alive. Every signal requires a live
group leader with the recorded PID and kernel start time. macOS end notifications use fixed
AppleScript with data passed as argv; notification failure is ignored.
Notifications are skipped outside macOS and for an unknown exit code.
Every terminal outcome writes an empty private signals/<agent>.<goal_id>.ended
file through atomic exclusive creation; duplicate names coalesce. Recovery
also writes a signal. A normal completion's signal persistence failure is audited.

Running runs with a current regular report file older than five minutes, or a last
event older than fifteen minutes, show display_state `stuck?`. Both comparisons
are strictly greater than the threshold. Missing events use the run start time;
linked files do not supply timestamps. No automatic cancel is added.
Supervision checks once a second; status and signal-list calls also detect it.
The first detection exclusively creates an empty private
signals/<lane>.<goal_id>.stuck using the existing signal mechanism. Metadata
persists stuck_signalled per run, so acknowledgement and restart do not repeat
delivery. Reused lane/goal pending names coalesce as with end signals.

Any live manager observes recorded processes on normal polls. A dead run becomes
ended with unknown exit and report-derived completed/ended_no_report outcome.
Verified survivors regain in-process checks under an exclusive per-run lock.
Darwin uses the kernel's microsecond start time; Linux uses boot ID plus start
ticks. Mismatched identities are never signalled and become ended with unknown
exit. Legacy records with a live PID but no identity remain busy and unsignalable.
Never infer an exit code. While all servers are down timeout supervision is absent.
LaunchOnlyOnce supplies OS cleanup when the Darwin agent exits while MCP is down;
current macOS app-quit survival and that cleanup remain owner-pilot checks.

MCP startup and the explicit recover command enable crash recovery. Display
commands never launch. An unreported end without cancellation/timeout becomes
interrupted and raises lane.goal.interrupted. A frozen marker blocks dispatch
but leaves interrupted goals visible. With auto_resume true, fewer than two
recovery attempts, a configured free lane and no freeze, recovery uses the
recorded agent session or starts the original brief fresh. The prescribed
restart note precedes the brief. A busy lane keeps its goal waiting; MCP polls
retry when it frees. Child and parent links repair a crash between their writes.
Signals raise lane.goal.resumed and persist acknowledgement-safe delivery flags.
Status, agents, history and web show interrupted, waiting to resume and resumed
counts. Active recovery reads `Running · resumed n/2`; finished recovery reads
its outcome with the count (`Ended`, `Failed`, `Cancelled` or `Timed out`
` · resumed n/2`). Status and history JSON carry an optional `badge` tone
(green = working or finished normally, amber = needs a look, red = failed) that
the dashboard applies without parsing label text. History exposes chain IDs and count, never the private session ID.
Interrupted records beyond the limit stay visible for owner action. Nothing of
nuntius runs while all nuntius processes are stopped. The Mac decides sleep;
reboot/power loss recovery occurs at the next MCP startup or recover command.
Malformed/integrity-failed records and partial temporary files are ignored;
unsafe links and I/O failures remain errors, invalid config remains non-serving.

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
  Running lanes add current_action: summary, age_seconds, age_source and optional
  running_seconds until a matching command completion/tool result. IDs are short.
- nuntius_history(limit=20, lane, goal_id, since): read-only finished-run list,
  select newest completions, then group by recorded chain, limit 1..200. Optional exact lane/goal filters combine;
  since is a positive Go duration (e.g. 24h) relative to completion time.
  Rows include run_id, lane, goal_id, ended (timestamp), outcome (redacted,
  at most 256 bytes), duration_seconds (number), report_exists (boolean), exit
  (number or null), relation, short_run_id, chain_id and chain_depth. MCP IDs and
  link IDs are the first eight characters; only CLI --json has full IDs.
  Running and undated records are excluded. No private run
  text or process/session identity is returned. Empty results are [].
- nuntius_dispatch(lane, goal_id, text): start when idle; return run_id.
- nuntius_reply(lane, text): resume the ended last run when its session ID exists.
- nuntius_tail(lane, lines=50): 1..200 one-line action summaries, each at most
  120 Unicode characters; no tool output/results or edit bodies. The last
  complete-event window is at most 1 MiB.
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
- nuntius_signals(): only a sorted JSON array of pending ended/stuck signal names.
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
existence requires a regular goal report modified at or after that run started;
an older report with the same goal ID is ignored. A later dispatch owns reports
at or after its start, so old history does not borrow the new report. Late
report delivery after an end is accepted until that next dispatch.

Returned strings redact GitHub token prefixes, sk-/sk-ant- API keys, xox Slack
tokens, JWT-shaped strings, PEM private-key blocks (including incomplete blocks),
and lines containing password=/secret=/token= assignments, ignoring case and
spaces around the equals sign. Redaction precedes truncation and line slicing.

Session IDs are recognized as top-level session_id/thread_id or nested
session.id/thread.id in the first 1 MiB of complete events. IDs must be 1..128
safe ASCII identifier characters. Unknown shapes disable resume. Real event
shapes and Desktop permission interaction remain owner-pilot questions.

## CLI and validation

Commands: mcp, status [--long] [--json], agents [--json],
history [--json] [--limit N] [--lane L] [--goal ID] [--since 24h],
audit [--since 1h] [--json], paths, web [--bind 127.0.0.1:PORT] [--tailnet] [--tailnet-optional] [--tailnet-keep] [--notify-imessage HANDLE] [--allow-host HOST], unfreeze,
recover, watch LANE, config check, config convert, set KIND-agent-pool N, version and help. Global config
and store flags precede the command. Status and agents default to human views;
--json preserves their machine shapes. MCP output stays JSON. Status columns
use optional display_state for cancelled, timed_out and stuck?, while retaining
legacy machine state values. Status also carries launchd_label and launch_fallback
when applicable; human status shows a fixed warning for fallback runs.
History defaults to the last 20 completed runs;
its table has ENDED (local time), AGENT, RUN, GOAL, OUTCOME, DURATION, REPORT yes/no,
EXIT (- for unknown) and RELATION, using the same redaction and duration helpers.
Relations name continuation start times, interruption times and successor start
times, never opaque IDs. Chains group together and descendants are indented.
Limits/filters select rows before grouping; missing relatives get an honest
earlier-run label. Cycles and cross-goal/lane links cannot extend ancestry.
History JSON preserves numeric/boolean/null types and full IDs; MCP has short IDs.
Status columns
show name/state/short run ID/goal/elapsed/ETA/event age/activity/report; the header shows
freeze state and data directory. Idle agents become frozen in the human view
only. ETA needs at least three earlier completed same-kind/same-size runs;
goal size comes only from the first line. Median minus elapsed is shown as
approximate minutes left, or minutes over the median. Missing history gives -.
Activity uses fixed labels without command arguments. No colour escapes.
Status --long adds the latest shared action summary and its age. All running
CLI JSON/MCP/web lanes have current_action. Native streams omit wall times, so
age_source observed uses first observation, initially estimated from file mtime;
human/web ages carry ~. Timestamped events use age_source event. Matching
command starts/completions and Claude tool_use/tool_result IDs govern running
duration; output/results never enter summaries or reset an observed action age.
Observation clocks are in memory and reset on reader restart. Actions outside
the last 1 MiB of complete events may be unavailable. No launcher or supervision
behaviour changes to obtain event timestamps.

One actionSummary function supplies watch, MCP/web tails and current action.
Claude Bash uses command's first line; Edit/Write/MultiEdit/NotebookEdit use verb
and path; Read uses read/path; Grep/Glob use pattern/path; WebFetch/WebSearch and
ToolSearch use URL/query; MCP uses server.tool/key argument; Task/Agent use
description. Codex command_execution uses command, file_change lists each
add/update/delete path, mcp_tool_call uses server.tool/key argument, web_search
uses query, reasoning/agent_message use first text line. Unknown tools/items use
name/type and the first eligible string argument. Redact precedes first-line
selection, control removal and the 120-character Unicode bound. Private-key
blocks spanning text events remain redacted. The no-report final-message
exception retains its existing bounds. Full run IDs occur only in CLI --json;
other views and MCP responses use the first eight characters.
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
start URL and required in the X-Nuntius-Token header on /data and /action.
The static shell is public and contains no private data or credential.
Host must be 127.0.0.1 or localhost
with the listener port; POST Origin must equal the local HTTP origin. A single
--allow-host opts in one validated DNS name ending in .ts.net (no wildcards,
ports, paths or schemes). That exact Host, with no port or :443, also passes;
its POST Origin must be exactly https://HOST. Forwarded and X-Forwarded-* grant
nothing. Binding stays loopback-only. Startup prints both private sign-in links
and a terminal QR of the HTTPS link, with an explicit white quiet zone.
The owner supplies tailnet-only HTTPS through Tailscale Serve, never Funnel;
the Mac must be awake. With --tailnet, nuntius finds the Tailscale CLI (PATH,
then the app bundle), requires BackendState Running, takes Self.DNSName without
its trailing dot as the allow host and requires it in CertDomains; each failure
is one sentence naming what to turn on, and --tailnet-optional falls back to
loopback. It reads serve status, reuses an https:443 mapping that already
proxies to the dashboard's loopback port, otherwise runs
`tailscale serve --bg --https=443 localhost:PORT` and confirms it; anything
else on 443 or Funnel anywhere on the node is refused with the exact command
to run, and nuntius never enables or disables Funnel. On a normal exit it runs
`tailscale serve --https=443 off` only for a mapping it created this run;
--tailnet-keep leaves it. No other state-changing tailscale command is used.
--notify-imessage HANDLE, or the notify_imessage config key, sends one message
"nuntius dashboard (NAME): LINK" through osascript with a fixed AppleScript
that reads handle and text from argv; handles are email or E.164 only; a failed
send warns and the dashboard keeps running. The link is never written to a
nuntius log or history file. Background web start stores the credential only in
web-access.json (0600) in the owner's data directory (0700); stop removes it.
web.json records the dashboard PID/start identity, address, job, start time,
tailnet host, serve mode and last iMessage result for web status and status --long.
Foreground web keeps its credential only in memory. Start prints one summary
line (tailnet host, serve none/reused/created/manual, iMessage off/sent/failed)
without the credential. web start uses a one-shot user launchd job on macOS,
no RunAtLoad/KeepAlive, inherited umask, private file streams and an in-memory
environment FIFO; Linux uses setsid and web.pid. It waits for loopback readiness,
refuses duplicates and supports --replace. stop verifies PID/start identity,
shuts down cleanly and removes its recorded job. link [--notify] displays owner
access again and optionally resends. QR uses qr.L, half-block rows, two-module
quiet zone and a terminal-width guard. Tests decode it offline and use fixture
launchctl, tailscale and osascript programs on PATH. Actions
accept only POST with the credential in a header. No CORS, external assets or
access logs. The browser stores the start credential in sessionStorage for this
tab, removes it from the address bar and reads storage on reload. Empty storage
shows instructions to reopen the printed start link. Disabled storage permits
the current start-link load only. Query credentials cannot authenticate APIs.

The page is embedded as index.html, app.css, app.js and the CC0 Relay SVGs.
The favicon and theme-specific mark are public static assets under the same
Host/method guards, with img-src 'self'. Asset GET requests pass
the same Host, port, method and response-header checks as the page. Script and style CSP
directives are 'self'; no inline script/style or external network asset is used.
Static asset URLs carry no credential. All existing response headers remain.

The responsive light/dark dashboard keeps freeze controls, running/idle/ended
counts and refresh time in a sticky header. Accessible tabs select Agents
(default), Messages & notes, Digest, History or Audit. The URL hash restores
the selected view on start and reload. Arrow keys and
Home/End move tab focus and selection; hidden panels defer updates until shown.
Tab counts show running agents, unread notes and runs with open QUESTIONS.
Agents adds All and per-lane sub-tabs, keyed by #agents/LANE, with browser
history navigation and runtime pool reconciliation. A lane shows its current
card, newest-first live tail with a Follow control, its rows from the latest
50 dashboard runs and report links, addressed owner notes and pending signals.
The strip scrolls horizontally on phones and uses the server badge colour.
Compact two-column desktop cards include agent kind/model/effort, state text/colour,
goal, short run ID, human elapsed time, ETA, event age, current action and report status.
Only running cards offer cancel, with a two-step in-page confirmation.
The initial cancel button hides until Keep running or the confirmation timeout.
Every card's Message button selects that agent in the Messages composer.
Phone history stacks lane/state, goal, run ID, started/duration and relation; badges, lanes and
durations do not split words. The phone tab bar scrolls within its own bounds.
The phone header has brand/freeze controls above counts/update age. At widths
up to 720px the pill shows Enabled/Frozen; title and aria-label retain the full
dispatch/freeze text. Panel focus
outlines require keyboard input and focus-visible. Initial and newly visible
panels render even with a recent hold; panel element focus alone is not busy.
Data adds kind, eta and activity only to web lanes, reusing the status helpers;
CLI/MCP retain legacy state values and add optional display_state. Web history
also has display_state for distinct terminal outcomes. Existing redaction and
text limits apply.

Audit is parsed in Go into typed, newest-first rows: local time, readable
action, lane, goal, ok/refused with reason, short digest and redacted raw line.
CLI audit shares that reader. Malformed/incomplete records are skipped; the
reader considers the latest 1 MiB and returns at most 200 entries within 64 KiB.
The dashboard filters by text/lane/action and hides each raw line in a toggle.
Owner notes may optionally name a configured lane; shared judge notes remain
in Messages & notes.

Two-second polling updates keyed cards and history rows in place. Expanded
reports, open tails and open audit content remain stable. Focus, selection and
recent section interaction defer updates; message drafts and lane selection
survive polling. Relative times expose absolute dates on hover. A freshness
indicator and visible error banner distinguish current data from stale data.

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
