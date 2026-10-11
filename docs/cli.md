## Lane holds and held messages

```sh
nuntius lane pause codex-1
nuntius lane pause claude-1 --after-current
nuntius lane resume codex-1
nuntius lane list
nuntius lane list --json
```

Use the exact configured name. Both pause forms block new work immediately;
the current run may finish. Pause sends no stop signal and does not extend its
timeout. Cancel still stops that run. Human output explains the hold separately
from its run state, failed outcome, report and attention badge, for example:
“Paused by you at 14:10 via CLI; current run may finish”. Times use your local clock.

Resume saves a clear hold and starts no work. It does not unfreeze, recover a
goal, deliver a message or recreate a removed lane. `lane list` includes saved
names removed from configuration, sorted by name. Re-adding the same name,
even with a different agent kind, retains its saved hold.

These commands use the existing global `--config FILE` and `--store DIRECTORY`
selectors before the command, and the same environment defaults. They never
rewrite configuration. Only pause/resume initialize missing control storage.
`lane list`, CLI `status` and `agents` create no store, lock, signal or run file,
reconcile no intent and launch no recovery or supervision. Unknown or unreadable
controls appear as unavailable and block new work; the other run facts remain
visible. `lane list` exits 1 when it cannot confirm the controls, while still
printing that safe snapshot. Mutation exits are 0 for a durable changed or
unchanged result, 1 for refusal or uncertainty, and 2 for invalid syntax.
An uncertain response is not proof of a save or a delivery. Read the current
state before deciding whether to retry.

JSON supplies `lane_control_version: 1`. Each safe control has `paused`,
`phase` (enabled, draining, paused or unavailable), UTC `since`, `source`,
`revision`, `configured`, `dispatch_eligible`, fixed `blockers` and a plain
`explanation`. Status includes the full sorted control snapshot under
`lane_control`; status/agents rows add their own `lane_control` without replacing
run `state`, `outcome`, `badge` or `report_exists`. A draining lane can be both
running and paused. Availability also accounts for freeze and held work.
Status and list never contain held message text or session values.

To review a held message, use its full original run ID from status/history:

```sh
nuntius lane held review codex-1 ORIGINAL_RUN_ID
```

The explicit review returns only safe identifiers, the content revision and
digest, state, and at most 8 KiB of redacted text. It changes no files. Use that
exact revision and digest with `lane held send codex-1 ORIGINAL_RUN_ID --revision
REVISION --digest SHA256`, or replace `send` with `discard`. These names in
capitals describe values from your review; they are not literal arguments.
Send requires the original session and agent kind, a clear hold, no freeze and
an idle lane. Changed content or the wrong original run is refused. Delivery
uses a durable receipt; repeating the same confirmed send does not start another
child. Discard deletes only the selected held content and does not cancel a
possible child. An uncertain delivery stays reserved for explicit investigation.
Resume never consumes held messages.

## Run views

```sh
nuntius config check
nuntius status
nuntius status --long
nuntius status --json
nuntius history
nuntius history --json --limit 50 --lane codex-1 --since 24h
nuntius audit --since 1h
nuntius audit --json
nuntius paths
nuntius watch claude-1
```

`status` and `agents` print human views by default; add `--json` for machine
output. Status JSON keeps existing fields and adds `current_action` for running lanes.
The aligned status view shows agent, state, short run ID, goal, elapsed, ETA, last event age,
last activity kind, report presence, kind, model and effort. Its header shows freeze state and the
data directory. Idle agents show `frozen` during a freeze; active and ended
agents keep their run state. No terminal colour escapes are emitted.

`agents` displays effective launch flags separately, for example
`claude-1 ... model=claude-opus-5-5 effort=high` and
`codex-1 ... model=gpt-6.1-sol effort=default`.
`status --long` includes MODEL/EFFORT columns such as
`model=claude-opus-5-5 effort=high`, plus the current action.
`--model`/`-m` and Claude `--effort` accept split or `=` forms. A missing model
is `default`. Codex effort comes only from structured startup metadata when
present; `default` means the stream did not report it. Nuntius never reads
`~/.codex` or other agent settings. OpenCode keeps its template model.
Status for a recorded run and history use that run's launch values, not the
current template. Old records without launch identity show the current template values with
`(configured)`; they are not a claim about the historical launch.

`audit [--since 1h] [--json]` shows the latest bounded actions, newest first,
with local timestamps, action in words, lane, goal, result and a short brief
digest. Refusals include the recorded reason; older refusals without one say
`reason not recorded`. JSON includes structured fields and the redacted raw
record. The reader uses the latest 1 MiB of complete audit lines and returns
at most 200 entries within a 64 KiB presentation budget.

ETA uses the first line's `size S`, `size M` or `size L` and the median duration
of earlier finished runs of the same agent kind and size. Only runs that ended
before the current run started count. With fewer than three matches or unavailable
history it shows `-`; otherwise it shows `~12m left (n=5)` or
`over usual (+8m)`. It is an estimate, never a dispatch or waiting rule.
Activity shows fixed kinds such as `editing files`, `running tests` and
`git push`, never tool arguments or command text. Unknown kinds use a generic label.

`status --long` also shows the latest action: the actual command's first line,
edited/read file, search query or tool argument. Running lanes expose the same
`current_action` object in MCP and on the dashboard: `summary`, `age_seconds`,
`age_source`, and `running_seconds` while a matched command has not finished.
An event timestamp gives an exact age (`age_source: event`). Native streams
usually omit timestamps; ages then use the first observation, initially
estimated from event-file modification time (`age_source: observed`, shown
with `~`). Unrelated output does not reset an observed action's age. Observation
times live only in memory and restart with the reader. Commands lacking a start
event or identity do not claim a running duration. Parsing uses the latest 1 MiB
of complete events; an action outside that window can be unavailable.

`history [--json] [--limit N] [--lane L] [--goal ID] [--since 24h]` shows
finished runs, including cancelled, timed-out and no-report runs. It defaults
to the last 20, newest completion first; `--limit` accepts 1..200. Lane and
goal filters are exact and can be combined. `--since` is a positive Go duration
measured from now against completion time. The human table shows ENDED in local
time, AGENT, RUN, GOAL, OUTCOME, DURATION, REPORT (yes/no), EXIT (`-` if unknown)
RELATION, KIND and MODEL. Relations say `first run`, `continue step of the 01:04 run`,
`resumed 1/2 after interruption at 23:31`, or `resumed by the 23:40 run`.
The selected runs group by their recorded chain, newest chain first, with child
runs indented. Separate goals that reuse the same name are not merged.
JSON retains timestamps, numeric seconds/exit codes, booleans and null unknown
exits; it excludes session IDs, process identity, prompts and report text.
CLI `--json` alone retains full run/link/chain IDs. Human status/history, the
dashboard and MCP use the same first eight characters. MCP history adds readable
`relation`, `chain_id` and `chain_depth`; CLI JSON also supplies `short_run_id`.
MCP dispatch/reply responses use the same short run ID; actions still target lanes.

Human status, agents and the dashboard distinguish `timed_out` and `cancelled`.
An active recovery run reads `Running · resumed 1/2` (or `2/2`), so the
resume count does not hide that the agent is running. A finished recovery run
reads as its outcome: `Ended · resumed 1/2`, `Failed · resumed 1/2`,
`Cancelled · resumed 1/2` or `Timed out · resumed 1/2`, in status,
`status --long`, history, MCP status/history and the dashboard.
Existing status/agents JSON `state` values remain compatible; an optional
`display_state` supplies those labels and `stuck?`, and an optional `badge`
names the dashboard colour (`green`, `amber`, `red`; absent for plain). A running run is `stuck?`
when its regular report file's modification time is over five minutes old,
or its last event is over fifteen minutes old. With no event file, age starts
at run start. Checks use strict thresholds and never automatically cancel.
Supervision checks once a second; status and signals also check immediately.
Only runs that are still running qualify. Reused goal IDs can refer to older
reports, so use distinct goal IDs as with report-presence checks.

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
`watch`, MCP `nuntius_tail` and dashboard tails share one action-summary parser.
Commands show their first line; edits show a verb and file; searches show their
pattern/query; MCP tools show `server.tool` and a key argument; agent tasks show
their description. Codex file changes show add/update/delete with each path.
Unknown tools/items show their name and first eligible string argument.
Summaries are redacted before truncation, stay on one line and contain at most
120 Unicode characters. Tool output/results and edit bodies are excluded.
`watch` prints changed tail snapshots until the lane ends; it is not a raw
transcript. Ctrl-C stops watching. The existing ended-without-report exception
still returns the bounded final assistant message.

## Measured CLI interface

Measured using help only on 2026-10-09: Claude Code 2.1.295 and Codex CLI
0.159.2. That measurement did not verify real event shapes. For v0.2.5,
`testdata/action-events.jsonl` captures sanitized action envelopes from existing
Claude stream-json and Codex exec --json run files. Its provenance note describes
the replaced arguments and excluded private data. Tests replay these fixtures
offline; this change does not start a real agent.

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
events, unfamiliar event types, cancellation, timeout or an error result suppress
the automatic resume. A complete result event is required. Each run's metadata links the other run. The lane lock
stays held between them. A manual reply never triggers another automatic reply.


## Version and ETA provenance

`nuntius version`, `nuntius --version` and `nuntius -v` print the same version.
Running status JSON/MCP/dashboard lanes include eta_details when an estimate
is available: median_seconds, samples, kind, size, label and explanation.
The dashboard keeps the short label; hover or tap for its explanation.
`status --long` also prints the explanation.
