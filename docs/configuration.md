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
Codex and OpenCode support `as_is` only. The text and start mode cannot be supplied by MCP.
`auto_resume` is optional and defaults to true. Set it to false to keep
interrupted goals visible without automatically starting another agent.
`notify_imessage` is optional: your own iMessage handle (an email address or an
E.164 phone number such as `+15551234567`) that `nuntius web --tailnet` sends
the phone sign-in link to. `--notify-imessage` on the command line overrides it.
An invalid handle is refused at load. See "Phone access over Tailscale".

Each kind has one template and a size from 0 to 8. Agents are named
`claude-1` through `claude-N`, `codex-1` through `codex-N`, and `opencode-1` through `opencode-N`. A disabled
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

`extra_args` supports `--model`/`-m` for Claude and Codex, and `--effort` and
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

## Idle observation (opt-in, alert_only)

`idle_guard` accepts only `"alert_only"` or an empty string. Omission and the
empty string disable the observer. Duplicate keys, case aliases, null and
execution modes are refused. The owner edits this local field; no Settings,
MCP or browser operation enables it. After independent review, an explicitly
started `nuntius observe` process owns observation until that process stops.
No service is installed, restarted or activated by saving configuration.

The observer samples at startup and every 60 seconds, independently of model
responses and browser activity. It opens the existing bounded portable Queue
snapshot read-only and treats its ready rows as conservative hints, never as
native readiness, approval, write scope, work class or permission to start.
Missing, invalid, stale (over two minutes), future-dated or incomplete input
produces unknown coverage. A complete empty queue is blocked/no-demand time.
Goal-local owner holds do not hide unrelated ready rows. Uncertain native
reservations, held successors, pending recovery and incomplete run inventory
retain capacity; a confirmed ended run without such holds may be free.

Only fresh candidate-complete OWNER_HOLD_ALL, LOAD_ABOVE_LIMIT, CLAIM_HELD or
FILE_CONFLICT proofs can permit idle time. S1 has no installed trusted proof
producer: Queue text cannot supply these proofs, claims, canonical scopes or
heavy-work classification. Unattributed lane pauses/freezes remain unknown;
the observer never clears them. The internal proof seam requires trusted
capture, policy revision, exact lane/candidate mapping, evidence digest,
unexpired proof and observation no older than 30 seconds. Load proof applies
only to heavy work with consecutive high samples; lower safe work defeats
any partial exemption. No host-load or claim authority is guessed from hints.

`idle-guard.lock` fences one observer per private store. `idle-guard.journal`
retains checksummed bounded frames (16 MiB, 20,160 frames, 256 KiB per frame).
It refuses saturation, truncation, corruption, links and replaced controller
inodes without evicting or repairing history. Interval and incident commits
sync before a notification attempt. The notifier is an offline injectable
interface; S1 installs no transport and never sends to a phone.

One incident names each continuous violation after five known unexplained
minutes. Restart preserves its identity and accumulated grace. Unknown gaps
do not resolve it or earn grace; a notification acknowledgement cannot resolve
an ongoing violation. Failed or uncertain persistence stops sampling and
keeps the daemon alive with a fixed local fault diagnostic. Readers report
stale/missing coverage and never repair records. The process epoch and UTC
heartbeat survive in the journal; cross-process boot/clock continuity is not
asserted. The first restart gap is unknown, even when short. Within a process,
grace uses monotonic elapsed time. Sampling gaps over two periods and rollback
are unknown; rollback cannot create overlapping report intervals.

## opencode lanes

Install [OpenCode](https://opencode.ai/docs/) separately and log in using its
own tools. Login stays in OpenCode's own configuration; nuntius never reads or
copies it. Add an owner-chosen provider/model to the pool template:

```json
{
  "pools": {
    "opencode": {
      "size": 1,
      "template": {
        "workdir": "/absolute/workspace/repository",
        "binary": "/absolute/bin/opencode",
        "model": "opencode/minimax-m2.7",
        "extra_args": [],
        "start_mode": "as_is"
      }
    }
  },
  "max_runtime": "8h",
  "reports_dir": "/absolute/workspace/reports",
  "notifications": true
}
```

The model is an example, not a free-model claim or a default. Choose the exact
OpenCode Zen model ID and check its current cost with OpenCode. Nuntius requires
an explicit `provider/model` and never chooses, upgrades or falls back to another
model. Keep it in the template's `model` field; `--model` in `extra_args` is
refused to prevent conflicting selections. Optional `--agent` and `--variant`
accept split or equals forms. No MCP request can select a model or change flags.

For Identuum, follow the owner-install steps in
`wiki/tools/opencode/README.md` for the guard plugin and
`wiki/tools/opencode/opencode.permission.json` template. OPENCODE-GUARD-1 owns
those files. Install the plugin in the documented plural `plugins/` directory
and merge only the permission member, preserving the owner's other settings.
Review effective agent overrides and launch inside the configured repository
so its parent `AGENTS.md` rules apply. Nuntius installs or edits none of these.
The guard is a tool hook, not an OS sandbox; see its guide for containment limits.

Run `nuntius config check`, then `nuntius set opencode-agent-pool 1` or
`nuntius agents` to inspect the pool. The template must already exist. Legacy
OpenCode lanes also support `config convert`; different models within one kind
refuse conversion. Status, agents, history, MCP and the dashboard show kind and
model and effort; historical runs retain their launch values after configuration changes.

Starts use `opencode run --format json --model MODEL [extra_args]`, with the
brief supplied intact on stdin. Resume adds `--session ID`; missing IDs use the
same brief with the existing interruption note during crash recovery. Darwin
uses the same transient launchd job and identity checks as other agents. Cancel,
timeout, freeze and the bounded recovery chain follow the same rules.

Verified CLI help: OpenCode 1.18.35. Its `--auto` flag automatically approves
permissions not denied, but nuntius refuses permission overrides, including
`--auto`, as well as `--pure`, attachments, remote-server selection, interactive
mode and output/session overrides. Configure permitted mechanical tools in
OpenCode's normal permissions instead. Nuntius inherits the environment without
injecting `OPENCODE_PERMISSION` or changing settings.

Event fixtures are derived from
[upstream run source](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/cli/cmd/run.ts),
not a live model capture. They remain unverified against real 1.18.35 model
output. An isolated free-only Step 5 attempt returned an error event and no
successful model text; no paid model or owner configuration was supplied.
The documented JSON stream supplies `sessionID`, millisecond timestamps,
text, step events, errors and nested tool state/input. Summaries exclude results.
Current upstream emits tool parts at completion/error only, so running-command
duration is unavailable without a start event. Native MCP names use
`server_tool`; display splits at the first underscore, and server names containing
underscores can be ambiguous. Unknown event types retain a bounded first string
argument, excluding identity/result metadata. Raw private streams stay on disk.


## Notification toggle

Set your own recipient in notify_imessage in the owner file. Then use
`nuntius set notify-imessage off` or `on` without losing that recipient.
The optional notify_imessage_enabled boolean records this choice; omitted means
enabled when a recipient exists. Enabling with no recipient is refused.
`set --json` exposes display settings without recipient or template arguments.
The dashboard Settings page uses the same Manager policy and private writer as
these CLI setters. It can change existing pool sizes, this boolean and the
saved phone-access choice.
Templates, recipients, paths and execution options remain local-only.

## Phone access

`phone_access` is an optional JSON boolean with one exact decoded key spelling.
Duplicate occurrences, case aliases, null and coerced values are refused;
one escaped canonical spelling is accepted. Omitted or false keeps normal
dashboard startup local. True requests strict Tailscale startup, including
when you choose Start dashboard in the menu bar. Save it in signed-in Settings
or use `nuntius set phone-access on` or `off`. Stop and start the dashboard
locally to apply it; saving never restarts a service or sends a link.
The menu shows the running dashboard's actual readiness separately from the
saved next-start choice. If it is already running locally, the menu explains
that a restart is needed. Explicit networking flags override the saved choice.
Explicit judge recovery still requires verified Tailscale access.

The recipient stays local-only. Menu sends read the latest configured handle,
unless the dashboard started with an explicit recipient override. Recovery
retains the private handle but suppresses the child's automatic send; the
parent sends only after verification. Changing phone access does not enable
iMessage or edit a recipient. A Settings batch still changes at most four
fields; save a larger set in separate batches.

Lane holds are operational controls, not configuration settings. `lane pause`,
`lane resume`, `lane list` and held-message commands leave the selected lane
configuration byte for byte unchanged. Their private state belongs to the
selected data directory in `lane-control.json`, with a separate durable intent
for a save in progress. Do not edit these records by hand.
Shrinking or disabling a pool does not erase a saved name or hold. Re-adding
that exact name, including with a different agent kind, retains the hold.
`lane list` shows removed names as unconfigured and unavailable for dispatch.
You can resume their retained hold without growing the pool or recreating a
template. Resume starts no work and leaves a global freeze intact.
Read-only CLI list/status/agents never create the data directory or reconcile
a save in progress; they report unreadable controls as unavailable.

`queue_file` (since v0.3.3) is local-only: omit it for `queue.json` under the
resolved data directory, use an empty string to disable Queue, or supply a
clean absolute regular-file path without symbolic links. An absent explicit
file never falls back. Settings reports only default/configured/disabled;
it cannot read back or edit the path. The coordinator publishes snapshots,
and Nuntius only reads them. See [queue-format.md](queue-format.md).

## Native brief seals (local, unpublished)

The internal brief compiler keeps the author's body and the owner's shared
standards as separate exact UTF-8 bytes. A trusted local caller supplies the
profile name and revision, an explicit separator, the standards marker, and
either an empty wrapper or `/goal` followed by a newline. It appends the
separator and standards once. It does not expand templates or execute text.
The compiler adds no owner configuration fields, local file reader, CLI,
MCP tool or browser operation. Later adapters must select text or registered
artifact keys; they must not turn this into an arbitrary path reader.

The first body line must start `GOAL: ID (size S)`, using the stored goal ID
and S, M or L size. Optional comma-separated header details may follow the
size inside the parentheses. A short description may follow the parentheses.
The header must remain one line. Nuntius does not infer identity or size from
other prose. Invalid UTF-8, a byte order mark anywhere, a `PLACEHOLDER` prefix,
a missing or repeated standards marker, and mismatched headers need review.
The final body, separator and standards together may use at most 128 KiB.
Nothing is truncated. The private encoded seal must also fit its existing
256 KiB storage limit, including JSON escaping.
Assembly refusals explain the cause and give safe assembled and dispatch
byte/code-point counts. Invalid UTF-8 makes the code-point count unavailable.
The refusal never prints the body, standards or supplied references.

The seal binds the goal spec and revision, profile, authority reference and
reviewed source observation together with body, standards, final and dispatch
SHA-256 digests. It records UTF-8 bytes and Unicode code points separately.
Dispatch measurements include the `/goal` wrapper when declared. These
measurements feed the internal delivery component described below. It enforces
the direct 12,000-byte limit, per-engine code-point cap and full-artifact rules.
Internal dispatch by approved goal ID is described below; automatic dispatch
and public native controls remain unavailable.

Approval needs the exact sealed revision and authority. It also compares a
freshly supplied source observation with the sealed observation. If they differ,
`preflight_review_required` explains that the source changed and needs review
before resealing and approval. Nuntius checks these references, not the truth
of repository facts or human consent. A reference is not authentication.

Changed body or standards bytes offer a newer source without editing the old
seal or approved bytes. Refresh explicitly creates a new goal revision, returns
it to drafted, and removes the old approval and exact automatic grant. Review
and approve that new revision separately. A failed refresh preserves the old
seal. Bookkeeping sealing and approval start no child and leave lane controls,
pause, Resume, held messages and legacy dispatch unchanged.

The writer and saved-history reader recompute the same complete seal and
approval predicates. Changing execution scope removes the old approval and
exact automatic grant, even if the old seal and decision history are unchanged.
Entering approved state needs a new reviewed decision with the matching seal,
authority and source observation. Notes and title or ordering edits keep existing
approval and history. Closed JSON keys must match after unescaping; case aliases,
duplicate decoded keys, null fields and unknown fields refuse approval.
Profile names/revisions/markers, authority and source observations must each
be one nonblank line of at most 128 UTF-8 bytes. Unicode line and paragraph
separators and U+0085 are rejected in these one-line fields.

## Native brief delivery (local, unpublished)

A trusted local adapter must explicitly select the lane engine, its positive
Unicode code-point cap and an existing artifact root that the lane can read.
There is no guessed root or default cap. This adds no configuration fields or
CLI, MCP or dashboard operation. Preparing delivery does not approve a goal,
start a child or bypass a pause. Internal dispatch reuses the same delivery
profile and the locked native admission check.

A direct prompt uses the exact sealed body, separator and standards, with the
declared wrapper. The complete prompt must fit both 12,000 UTF-8 bytes and the
selected engine's code-point cap. Bytes and code points are separate measures;
the wrapper counts toward both limits. Nothing is truncated.

If either limit is exceeded, delivery uses a short pointer with the goal header,
quoted absolute file path and full-text SHA-256 digest. The file contains the
exact sealed body, separator and standards once, without the dispatch wrapper.
The pointer includes its declared wrapper and must also fit both prompt limits.
The full file may use at most 128 KiB. A pointer does not append standards again.

The root must be an owner-owned 0700 directory. Every ancestor must be a real
directory owned by that user or root, without group or other write access;
protected sticky temporary directories may be ancestors. Links are refused.
Artifacts are owner-owned regular 0600 files with one hard link. Their names bind
the goal ID, revision and full digest. New writes use exclusive creation and
sync the file and directory before returning. Existing files must match the
exact sealed bytes; delivery never truncates, replaces, repairs or chmods them.
Reusing a file confirms its durability through the checked read descriptor,
including when another writer has written the bytes but has not synced them yet.
A partial write stays in place for review and refuses the next delivery.

The delivery record keeps the full and prompt digests, byte and code-point
sizes, selected engine and cap, and artifact path and file identity. New delivery,
retained retries and saved-record reads share the same validation rule. A saved
record cannot silently adopt a replacement file, even with identical bytes.
Later admission must check the record again because a successful check cannot
promise that a path stays readable. Internal dispatch retains this descriptor
and checks it again at the final start boundary and during held reconciliation.
A same-byte replacement inode cannot become a new approved delivery on retry.
A local permission check also cannot prove
visibility inside a real lane sandbox; that needs a separately authorized pilot.

Refresh creates a separate revision file and preserves the old one for resume.
This component never expires or deletes artifacts. Archival requires explicit
owner maintenance after checking that no resume still needs them. Unreadable
roots, unsafe files, changed identities or digests explain why review is needed;
errors do not include the private brief text. Approval still uses the existing
sealed revision, authority and source-observation rules.

Native dispatch reads approved direct or pointer bytes. It does not append the
shared standards or wrapper again, select a guessed owner root, alter lane
argv/profile settings, or accept a free-text replacement. Body or standards
changes require explicit resealing and fresh approval. There is no new native
configuration switch or public adapter in this local component.

Native dispatch ends are now recorded automatically and repaired by writable
startup/maintenance after failed journal or audit delivery. No new profile,
poll setting or automatic grant is required. Ended work stays open for judge
review. Internal native linked continuations retain that approval and scope.

Internal native end observation honors `start_mode`/`continue_text` when
classifying an eligible first Claude acknowledgement and `auto_resume` when
classifying a new interrupted recovery hold. These classifications alone start
no work. Disabling recovery later does not clear an already saved pending hold.
Internal successor launch also requires an in-process trusted resolver with
fresh source, capability, physical root and complete claim observations at
selection and before start. No setting or stored digest supplies that authority.
The same path accounts explicit Reply and exact held Send/Discard without
adding a public native adapter.
