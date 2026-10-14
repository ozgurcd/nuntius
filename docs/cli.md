## Finite judge reads

```
nuntius queue [--state STATE] [--project ID] [--json]
nuntius report show ID [--section NAME] [--json]
nuntius tail LANE [--lines N] [--json]
nuntius status [--long] [--json]
```

Global `--config FILE` and `--store DIRECTORY` precede these commands.
`queue` reads the existing portable v1 snapshot. States are `planned`,
`blocked`, `ready` and `parked`; repeat `--state` for OR filtering. Rows keep
their original order and full ID. `total` and disjoint `counts` describe the
whole pending population; `matched` and `returned` describe the filtered set.
This display has `authority: false`. It makes no native scheduling claim.
`--project ID` returns `cannot_evaluate` / `unsupported_filter`: portable v1
has no project identity. No other design-stage filter is shipped.

`report show` reads the bounded redacted report through the existing reader
and overview parser. Section names are the exact §G names: `PLAIN SUMMARY`,
`STATUS`, `ITEMS`, `QUESTIONS`, `DECISIONS`, `IDS`, `HEADS`, `BRIEF'S ERRORS`,
`DENIALS`, `WIKI-DELTA`, `OPEN`, plus `CONSENSUS`. Quote names containing spaces.
Without a selector it returns the safe text and parsed sections. Duplicates
retain individual blocks and one-based inclusive line spans, with diagnostics.
The title and end marker must name the requested ID. CONSENSUS is exactly one
outer terminal `CONSENSUS: AGREE` or `CONSENSUS: DISAGREE R01 R02` line after
the closing report marker (and after an optional outer text fence). Fenced,
quoted, indented, duplicate, malformed or nonterminal lines are not verdicts.
Missing consensus stays missing. Redacted, unstable, oversized or conflicting
reports cannot provide a complete interpretation. Reading a verdict grants
no approval and does not certify a review loop.
Nonterminal `CONSENSUS:` lines stay in their original body section with a
diagnostic and an incomplete interpretation. Missing selectors return
`section_absent`; a missing verdict returns `consensus_missing`. These known
absences leave `data.incomplete` false when the source parse is complete.

`tail` returns one bounded action-summary window, then exits. `--lines` is
1..200, default 50. It uses the existing safe parser and ended-run fallback;
coverage is `bounded_window`, not a complete transcript. Missing store/run
evidence is unavailable; a known empty run set returns an empty window.

The three new reads emit one complete object with `--json`: schema
`nuntius.judge.v1`, command, mode, state_domain, result, observed_at, complete,
source {kind, revision, sequence, validity}, data, warnings and error.
Mode and state_domain are `portable` for queue, `report` for report show and
`run` for tail, including invalid-request and unavailable-source responses.
Unknown sequence/revision is null. A report revision hashes only the complete
accepted redacted text, never raw source. A queue revision hashes the complete
safe portable projection before filtering, including its publication time.
Both identify displayed evidence, not native store revisions. Links and private
text are excluded. The response ceiling is 1 MiB; overflow refuses rather than
trimming JSON. Exit 0 means a known complete read; 2 means invalid syntax or
unavailable/incomplete evidence; 1 means output could not be written.
Empty filtered results are successful. Errors use fixed codes without argv,
paths or source text. Flags and enums are closed; only `--state` repeats.

These reads, `audit`, `history`, `status` and `agents` use passive construction
and Close. They create no store, directories, control locks or audit entries,
and run no recovery, export, scheduler or agent. Independent report and tail
reads remain available when the optional Queue source fails. Existing
`status --json`, audit and history JSON retain their established formats.
History errors use a fixed diagnostic, without filesystem paths.
The shared overview now retains every recognised §G and legacy section in
source order; its duplicate-section diagnostics apply to all retained names.

## Lane holds and held messages

Phone startup uses the saved `phone_access` boolean when no network flag is
supplied. Use `nuntius set phone-access on|off`, or save the signed-in Settings
toggle, then stop and start locally. Explicit `--tailnet`, `--tailnet=false`
and manual `--allow-host` keep their existing meanings. Only an explicit
`--tailnet-optional` permits a loopback fallback, whose reason stays in the
menu. Saving a setting never starts a service or sends a link. Menu startup
uses this same CLI policy and never adds a sign-in link to argv.

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
controls appear as unavailable and block new work; contended locks appear as
busy, with new work still blocked. The other run facts remain
visible. `lane list` exits 1 when it cannot confirm the controls, while still
printing that safe snapshot. Mutation exits are 0 for a durable changed or
unchanged result, 1 for refusal or uncertainty, and 2 for invalid syntax.
An uncertain response is not proof of a save or a delivery. Read the current
state before deciding whether to retry.

JSON supplies `lane_control_version: 1`. Each safe control has `paused`,
`phase` (enabled, draining, paused, busy or unavailable), UTC `since`, `source`,
`revision`, `configured`, `dispatch_eligible`, fixed `blockers` and a plain
`explanation`. Status includes the full sorted control snapshot under
`lane_control`; status/agents rows add their own `lane_control` without replacing
run `state`, `outcome`, `badge` or `report_exists`. A draining lane can be both
running and paused. Availability also accounts for freeze and held work.
Status and list never contain held message text or session values.

Busy reads return promptly without reading mutable control records outside the
lock. Best-effort local `lsof` diagnostics share a 200 ms deadline and a bounded
output limit. When a holder's Homebrew executable path identifies an older
version, status names that version and asks you to restart the process after
upgrading. A missing tool, missing holder identity or unversioned executable
leaves the version explicitly unknown. No path or command output is displayed.
Nuntius never stops another process automatically. Writable startup initializes
coordination storage, logs a fixed error reason if recovery fails, and retries
pending recovery on its existing background tick. Read-only commands never repair it.

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

## Idle observation and morning view

With local `idle_guard` set to `alert_only`, `nuntius observe` runs a separate
long-lived observer without starting a model or requiring an open browser.
Stop this explicitly started process normally to end observation. Temporary
MCP/CLI managers and dashboard reads never start it. No installed service,
automatic dispatch, authority grant or phone transport is enabled.

`nuntius monitoring --since 24h` reads the bounded monitoring journal. Add
`--json` for raw seconds. Windows are half-open UTC, up to 31 days. Each current
configured lane partitions the requested window into running, permitted idle,
unexplained idle, blocked/no-demand and unknown time. Before the first sample,
after the last sample, on restart gaps, sleep gaps, rollback or unreadable
storage, time stays unknown. Reads append nothing and make no repairs.

The view prints O (known running plus unexplained opportunity seconds), U
(unexplained seconds within O), all five partitions and an observation ratio
`(O-U)/O`. O=0 prints N/A. Unknown seconds remain explicit and fail coverage;
they cannot improve the ratio. This is alert-only observation, not native
execution compliance or complete project health. Status shows monitoring
coverage, fixed faults and open-incident count when the mode is configured.

# Native goal bookkeeping

Global `--config FILE` and `--store DIRECTORY` selectors precede the command.
The `goal` namespace uses the native journal. The existing `queue`, `report
show`, and `tail` commands retain R01's portable/read interfaces; native rows
are not merged into the portable queue.

```text
goal create ID --op OP --stdin [--json]
goal update ID --revision N --op OP --stdin [--json]
goal seal ID --revision N --op OP --stdin [--json]
goal approve|verify|cancel|park ID --revision N --op OP --reason TEXT --authority ID --evidence ID [--json]
goal cover ID --revision N --op OP --by ID --reason TEXT [--json]
goal note ID --revision N --op OP --text TEXT [--evidence ID] [--json]
goal ended ID --run RUN [--json]
goal show ID [--json]
goal queue [--state STATE] [--search TEXT] [--json]
goal audit [--json]
dispatch ID --revision N --op OP --lane LANE [--json]
```

`verify` additionally requires `--attempt RUN --report-digest SHA256`, or
`--attempt RUN --alternate-evidence` with a named review evidence record.
`approve` optionally takes `--source-state VALUE` matching the sealed review.
Flags are closed per operation. Duplicate flags, extra positionals, arbitrary
state setters, coercions and post-command global selectors are refused.
Operation IDs are durable retry keys: repeat the exact request to obtain its
original receipt; reuse with different content is refused. Revisions are
positive decimal integers, except create's implicit expected revision zero.

Create and update read one complete typed goal spec from stdin, bounded by the
native spec limit. All required fields are present, including `after`, `writes`
and `resources` as JSON arrays (empty arrays are valid; null and strings are
refused). To add or remove dependencies, replace the `after` array in the full
spec and update with the current revision and a new operation ID. `goal show`
shows `depends_on`, dependents, specific blockers and full current/chain run IDs.
Cycles are refused with a deterministic cycle path. A cancelled dependency
remains blocked until an explicit spec edit removes it. `goal queue --search`
finds native open goals by ID/title; repeat `--state` for an OR filter over
drafted, approved, running, ended and parked. Parent counts remain in the
unfiltered snapshot beside the matched rows. Reads never recover or launch.

Seal reads one object with `body`, `standards`, `profile`, `authority`,
`source_state` and `reviewed_source_state`. Profile contains `name`, `revision`,
`separator`, `marker` and `wrapper`. Body/standards are JSON strings preserving
the exact brief bytes. No arbitrary file reader is exposed. Sealing changes the
revision and revokes prior approval. Approve requires the exact reviewed seal;
it does not create an owner execution grant. Cover uses the existing coverage
engine: the source stays parked until its target is verified, then closes with
that target's evidence. Cancelling a goal sends no process signal; an unsettled
attempt is refused. Lane pause/resume and lane Cancel remain separate controls.

Ended observes the exact latest recorded native run, including the full IDs
used by resume chains; it does not accept a judge-invented outcome. An old or
unsettled run is refused with expected/found details. Repeated ended and verify
with matching complete evidence are no-ops, leaving the journal unchanged.
Different evidence, operation reuse and stale revisions cannot use this path.
Native mutations mirror the existing audit. A durable commit whose audit mirror
cannot be confirmed returns `uncertain` with `committed_audit_pending` and its
receipt; inspect before repeating, then retry the exact operation.

Both output modes use the same safe values, with R01's redaction/link policy.
JSON schema is `nuntius.goal-cli.v1`; result is `ok`, `committed`, `no_op`,
`invalid_request`, `refused` or `uncertain`. Exit 0 is a successful read or
confirmed mutation/no-op; 2 is syntax/input refusal, 3 is application refusal,
4 is unavailable evidence or uncertain persistence, and 1 is output failure.
Refusals show expected/found revision and applicable state; bodies, paths and
internal error strings are not echoed. Responses are bounded complete JSON.

At this base, the executable has no trusted native source/scope/claim provider.
Dispatch-by-ID parses and delegates over the tested manager seam, but the
standalone CLI returns `trusted_dispatch_context_unavailable` before writes or
launch. Caller flags/stdin cannot supply that authority. Native queue still
returns known rows/counts with `uncertain` when capability/source/claim
observations are missing; it never claims complete startability. Production
activation requires a separately reviewed trusted provider integration.

The Python tracker remains a legacy migration source, not retired by B17:
`new`/writer metadata map to typed create/update, `dispatch` to sealed native
dispatch, `ended` to real run observation, `verify` to evidence-bound review,
`park`/`cancel` to typed decisions, `note` to append-only notes, and queue/plan
accounting to the journal projection and native audit. Portable publication,
legacy import/cutover, project ACTIVE/PAUSED/DONE decisions, idle reasons,
morning utilization reports, metrics export and pointer-brief delivery remain
in their existing tools. Native goals have no arbitrary `failed` state: run
failure is an observed outcome, followed by review/park/cancel. B17 does not
silently reinterpret those legacy operations or rewrite their records.
