# Portable queue format v1 (since v0.3.3)

Any coordinator can publish a complete JSON snapshot. Nuntius displays the
coordinator's plan; it does not approve, plan, resolve dependencies, execute
briefs or dispatch. Readiness does not prove an idle agent, a cleared freeze,
an acquired claim or authority to start work. Started and finished work belongs
in Agents and History, and the coordinator removes it from the next snapshot.

## Location and local configuration

Omit `queue_file` to read `queue.json` in the resolved data directory, selected
by `--store`, `NUNTIUS_DATA_DIR` and OS defaults. On macOS the default is
`~/Library/Application Support/nuntius`; on Linux it is `$XDG_DATA_HOME/nuntius`
or `~/.local/share/nuntius`. The configuration directory is never the default.
Set `"queue_file": ""` locally to disable Queue, or use an absolute, clean
local file path to override it. Paths with links or special files are refused;
an explicit missing path never falls back to the default. The local owner
sets the path and checks the configuration. Settings shows only its mode
(default/configured/disabled), with no browser editor or absolute queue path.

Nuntius opens the snapshot read-only. It never creates, repairs, truncates,
locks or deletes it. One coordinator owns each destination. For example, the
owner can point `queue_file` at `/srv/coordinator/queue.json`, then have the
coordinator publish there. A coordinator adapter should require its output
path as an argument and must not edit owner configuration.

## Document and goal schema

Use UTF-8 JSON: one object followed by EOF, no BOM, comments, duplicate keys
(including ignored keys), or trailing data. Known keys have the exact types
below; null is not omission. Limits count UTF-8 bytes. Maximum document size
is 1,048,576 bytes; maximum container nesting depth is 8, including ignored
fields. Unknown fields at every object level are ignored only for version 1
and are never returned or used for control decisions. Future versions fail.

| Document field | Required type and rule |
| --- | --- |
| version | Integer exactly 1. |
| updated | UTC RFC3339 with seconds: `YYYY-MM-DDTHH:MM:SSZ`, publication time. |
| goals | Array of 0–500 goal objects, the complete pending queue. |

| Goal field | Type and rule |
| --- | --- |
| id | Required unique case-sensitive string, 1–64 ASCII bytes, `^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`. A display identifier, not dispatch authority. |
| title | Required plain string, 1–240 bytes after trimming. |
| size | Required string S, M or L; coordinator-defined effort. |
| lane_kinds | Required array of 1–8 unique lowercase slugs, each `^[a-z][a-z0-9-]{0,31}$`. Unknown kinds remain labelled. |
| lane | Optional exact lane preference/restriction, using the ID grammar. It need not currently exist. |
| order | Required integer 0–2147483647; ascending, with ASCII ID ties. Gaps allowed. Filtering never renumbers it. |
| depends_on | Required array of 0–32 unique IDs; no self-dependency. Missing IDs are external/unresolved, never assumed complete. |
| needs_owner | Required boolean, independent of state. |
| owner_reason | Required nonblank plain string of at most 512 bytes when needs_owner=true; absent when false. |
| state | Required string planned, blocked, ready or parked. |
| reason | Optional plain string of at most 512 bytes; required nonblank for blocked and parked. |
| created, updated | Required UTC timestamps as above; created <= updated <= document.updated. |
| brief_sha256 | Optional string of 64 lowercase hexadecimal characters, digest of exact brief bytes. Provenance is not approval. |
| brief_link | Optional HTTPS URL of at most 2048 bytes; no userinfo, query, fragment or control characters. See link policy below. |

All display strings are plain text, not Markdown or HTML. Control characters,
including newlines, NUL and bidi override/isolate controls, fail validation.
One invalid goal invalidates the whole snapshot; Nuntius never silently drops
malformed goals and implies completeness. In-snapshot dependency cycles fail.

`planned` means still being prepared; `blocked` means the coordinator cannot
proceed, with a nonblank reason; `ready` means prepared with no owner hold or
dependencies; `parked` means explicitly deferred, with a nonblank reason.
Owner holds and prerequisites remain separate, visible concepts.

```json
{
  "version": 1,
  "updated": "2026-10-10T22:32:00Z",
  "goals": [{
    "id": "EXAMPLE-DOCS",
    "title": "Review the documentation",
    "size": "S",
    "lane_kinds": ["codex", "claude"],
    "order": 4,
    "depends_on": ["EXAMPLE-REVIEW"],
    "needs_owner": true,
    "owner_reason": "Approve the public scope.",
    "state": "blocked",
    "reason": "The review and owner decision are pending.",
    "created": "2026-10-10T22:28:00Z",
    "updated": "2026-10-10T22:28:00Z"
  }]
}
```

## Atomic producer protocol

Use an existing private directory (0700) owned by the coordinator/owner.
Validate the entire candidate before publishing. Exclusively create an
unpredictable same-directory temporary regular file with mode 0600 and no
link following. Write all bytes, flush and fsync, close, then atomically rename
over the destination and fsync its directory where supported. Never truncate
the destination in place or follow its symlink. Readers see complete old or
new snapshots. Publish `goals: []` to clear the queue.

Before rename, failure preserves the previous snapshot and removes only this
writer's temporary file. After rename, a directory-fsync failure means durability
is uncertain; do not claim rollback. Serialize coordinator events and exporters
before collection as well as publication. Nontransactional source files must
be checked for changes during collection; retry once or fail without publication.
Publication does not make a source ledger transactional. No cross-process CAS
is promised; serialization belongs to the coordinator.

## API, read limits and links

Authenticated GET `/queue` uses the existing exact Host/port, header-token,
method and POST Origin guards. It is absent from the public-shell allowlist.
GET needs no Origin. Nonempty queries/selectors fail; the browser cannot choose
a file, path or URL. POST/PUT/DELETE/PATCH/OPTIONS cannot read or mutate Queue.
No CORS, cache, filesystem traversal API, brief fetch, watcher, scheduler or
MCP tool is added. Existing no-store, nosniff and same-origin CSP apply.

The reader uses descriptor-rooted no-follow operations for every path component
and the leaf, nonblocking opens and a descriptor regular-file check. It refuses
symlinks, FIFOs, sockets, directories and devices. It reads at most 1,048,577
bytes and rejects oversize data before parsing. Detected changes during a read
fail safely. Read errors affect Queue only, never the operational data view.

The allowlisted, redacted response is `{status, version?, updated?, goals, reason?}`,
with goals always an array. `ok`, `not_configured` (absent default) and `disabled`
are HTTP 200. `missing` (absent override), `invalid`, and `unavailable` are HTTP
503 with fixed reason codes `file_missing`, `invalid_snapshot` and `read_unavailable`.
No original JSON, unknown field, source path or parser/OS error is returned.

Metadata is sensitive to every dashboard grant holder. Producers must omit
passwords, secrets, grants, cookies, sessions, prompts, execution details and
token/code/state URLs, including in ignored fields. Redaction is defence in
depth and cannot identify every secret.

Links are optional and untrusted. Reject non-HTTPS, localhost, `.localhost`,
`.local` and literal/encoded IP hosts, relative/data/javascript/file URLs,
credentials, queries and fragments. The UI validates again and opens a link
only on deliberate user action with noreferrer/noopener. Nuntius never fetches,
previews, proxies or downloads briefs. No local brief route exists.

See [dashboard behavior](dashboard.md#queue-since-v033) and [security](security.md).

## Native goal schema v1 (local, unpublished)

The native schema is an internal component. It does not create stored goals,
approve work, launch an agent, or add a CLI, MCP or dashboard operation. The
portable snapshot above remains a separate read-only display format.

Native creation data is one UTF-8 JSON object followed by EOF. Its `version`
is the integer `1`. Reject duplicate keys, unknown keys at every level, null,
wrong types, trailing data and documents over 65,536 bytes, including whitespace.
Numbers use integer notation, never strings, fractions or exponent notation.
Display text is plain text; controls and bidirectional formatting characters
are refused. Limits count UTF-8 bytes. No field is silently shortened or defaulted.
Unpaired escaped Unicode surrogates are refused rather than replaced during
decoding. Valid escaped surrogate pairs, literal escaped backslashes and the
actual Unicode replacement character are accepted.

Every spec requires these fields:

- `id`: the existing dispatch grammar, `^[A-Z0-9][A-Z0-9.-]{1,63}$`.
  This identity belongs to the goal and cannot be replaced by a later revision.
- `title`: 1–240 bytes, nonblank, with no surrounding whitespace; `size`: S, M or L.
- `lane_kind`: preferred engine, claude, codex or opencode. `kinds`: a nonempty
  unique set of allowed engines, at most 8 entries, including `lane_kind`.
  Pool names such as codex-1 are lane names, never engines.
- `work_kind`: mechanical, design, preparation, code, review or release.
  Work class and execution engine have different meanings.
- `lane_binding`: preferred or required. Optional `preferred_lane` follows
  the configured lane-name grammar, `^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`.
  A required binding needs a named lane and cannot silently fall back.
- `order`: integer 0–2147483647. Later selection sorts ascending, then by ASCII
  ID. Display filters do not change it.
- `after`: at most 32 unique dispatch-grammar IDs, with no self edge. An unknown
  prerequisite remains unresolved; decoding does not mark it complete.
- `writes`: at most 32 unique canonical workspace-relative paths, each at most
  240 bytes. An explicit empty array means read-only. Missing or null scope
  is invalid. Each slash-separated component matches
  `^[A-Za-z0-9_][A-Za-z0-9._-]*$`. Reject absolute paths, dot components,
  traversal, repeated/trailing slashes, backslashes, URLs, encodings and compound
  strings such as `repo other-repo`. List each scope separately.
- `resources`: at most 32 unique exclusive coordination names, each matching
  `^[a-z][a-z0-9-]{0,63}$`, for example mint or wiki-pin. An empty array is explicit.
  These are coordination claims, not paths or permission grants.

Optional `owner_hold` contains exactly `categories` and `reason`. Categories
are a nonempty unique list of at most 32 lowercase names using the resource-name
grammar; reason is nonblank plain text, at most 512 bytes. The names label
decisions still needed; they do not prove approval. Optional `coverage` contains
exactly `covered_by` (another dispatch-grammar ID) and a nonblank reason of at
most 512 bytes. The spec describes a hold, never acceptance by itself. Coverage
reasons also reject Unicode line and paragraph separators. The dependency and
coverage operation described below records closure separately.

Creation accepts no approval, actor, state, outcome, run, revision, pause,
brief path, report or closure evidence. Approval must be recorded separately
against the exact immutable brief and authority. A decoded path proves syntax
only. Internal admission resolves known workspace roots, rejects unknown scope,
and checks resource overlap and claims. No filesystem is read during decoding.
Lane-control, goal and projection revisions remain separate. Lane pause is
never a goal state; it remains under the existing lane-control authority.

The typed goal states are drafted, approved, running, ended, verified,
cancelled and parked. Outcomes are completed, timed_out, cancelled, interrupted,
report_delivery_failed, ended_no_report, unknown_exit and failed. Process failure
is an outcome, not another goal state. An unknown exit stays unknown; an ended
run is not verified work. These types define vocabulary, not transition logic.

The internal public DTO copies only the listed spec metadata plus typed state
and optional outcome supplied by a trusted record. It redacts titles and hold
reasons, detaches arrays and hold objects from the source, and refuses encoded
output over 32,768 bytes. It exposes no approval, full brief/report, private
absolute path, process/session identity, lane-control data or readiness claim.
Invalid data returns one fixed explanation without echoing input or parser errors.

```json
{
  "version": 1,
  "id": "EXAMPLE-DOCS",
  "title": "Review documentation",
  "size": "S",
  "lane_kind": "codex",
  "kinds": ["codex", "claude"],
  "work_kind": "review",
  "lane_binding": "preferred",
  "order": 4,
  "after": ["EXAMPLE-REVIEW"],
  "writes": [],
  "resources": []
}
```

## Native journal replay v1 (local, unpublished)

This internal reader reconstructs goal metadata from a private journal. It
adds no command, dashboard control, approval or agent launch.
Lane controls remain separate. Reading never creates a directory or lock,
repairs a tail, removes history or compacts the journal.

The owner-selected existing directory must have mode 0700 and belong to the
current user. The journal is `goal-journal.bin` inside that directory. An absent
journal in a valid directory means an empty projection; a missing or unsafe
directory is an error. Files must be regular, mode 0600, owned by the current
user and have one hard link. Descriptor-rooted no-follow opens reject linked
path components and leaves. Nonblocking opens reject special files without
waiting. Size and modification checks reject a detected change during reading.

Each frame contains an 84-byte header followed by a JSON event. Header fields
are, in byte order: four ASCII bytes `NGJF`; a big-endian 32-bit version `1`;
a big-endian 64-bit sequence; a big-endian 32-bit payload length; the 32-byte
SHA-256 of those first 20 header bytes; and the 32-byte SHA-256 of the payload.
The independent header checksum prevents a damaged length from making an
interior corrupt event appear to be an incomplete final append. Sequence starts
at 1 and advances by exactly 1. Repeated, backward or skipped sequences fail.
Checksums detect damaged bytes; they do not authenticate the same OS user.

The event is a closed UTF-8 JSON object with `kind` exactly `goal.spec`,
`goal_id` using the native dispatch grammar, `revision` in integer notation,
and `spec_ref`. An optional `seal_ref` must be a valid reference if present.
Reject duplicate or unknown keys, null, wrong types and trailing roots.
Each goal's revision starts at 1 and advances by exactly 1. Replay validates
every referenced historical spec, including superseded revisions, using the
strict native schema above. The spec ID must equal the event ID. A revision
cannot silently rename a goal. Later operations must enforce update and terminal
record policy; this reader does not grant mutation or admission authority.

References are exact flat filenames: `goal-spec-` or `goal-seal-` followed by
64 lowercase hexadecimal SHA-256 characters. Each name binds the exact bytes
of its private file. No absolute paths, traversal, arbitrary file selectors,
full brief, report, spec text or seal text belongs in an event. Replay checks
seal bytes and their digest as an opaque private reference; it does not approve
or interpret a seal. Missing, unsafe, oversized or mismatched blobs fail replay.

Limits per replay are 16,777,216 journal bytes, 4,096 payload bytes per frame,
16,384 complete events, 4,096 distinct goals, and 67,108,864 cumulative blob
bytes read, counting repeated historical references. Each spec is at most
65,536 bytes; each opaque seal is at most 262,144 bytes. Exact boundaries are
accepted. Overflow returns an error without deleting history or returning a
truncated projection. These component limits do not promise a future complete
public response will fit its separate response budget.

A valid incomplete final frame is uncommitted: replay returns only preceding
complete events, their last sequence and byte boundary, plus a torn-tail flag.
It does not load that frame's references. An invalid header, checksum, event,
sequence or spec returns no projection, even after a valid prefix. Unsupported
versions also fail. Reads never skip an interior event or guess a repair.
The fixed error explains that native history is unavailable or invalid and
asks the owner to preserve and review it; it echoes no private input or path.

The resulting internal projection owns immutable spec bytes and seal references.
Each metadata read returns detached arrays and holds; repeated replay of the
same files gives the same projection. Spec-only historical records without
coverage read as drafted. Coverage needs a parked record or the complete
derived change set described below. Record snapshots retain state and notes, but replay
itself provides no approval operation, lifecycle transition, readiness
calculation, current attempt or lane-control revision.
The internal commit component below saves changes before any native writer
can be exposed.

## Native journal commit (local, unpublished)

This component saves strictly validated spec bytes and optional opaque seal
bytes in the existing private store. It adds no CLI, MCP or HTTP operation,
approval, scheduler or process launch. It does not alter lane pause state.
The manager mutex comes first, followed by the existing shared control lock.
Control-lock contention refuses promptly; it never waits for another process.
No nested control or audit lock is taken by the commit itself. Audit delivery
uses the separate mirror component below.

Each change supplies the expected goal revision and an operation ID of 1–64
ASCII letters, digits, dots or hyphens, starting with a letter or digit. Zero
means the goal must not exist. A saved revision advances by one. A stale
revision refuses without staging blobs or appending an event. Operation IDs
are unique across this journal, rather than scoped to a goal. Reusing an ID
with the same input returns its original goal, revision and event sequence,
even after another update. Different input refuses. The input fingerprint
binds the goal ID, expected revision and exact spec/seal content addresses.
Changing JSON whitespace changes the input; an absent seal differs from an
empty seal. These are storage identities, not approval or authenticity proofs.

New events carry paired `operation_id` and `input_sha256` fields. Replay
refuses an invalid pair, fingerprint mismatch or repeated operation ID.
Earlier internal version-1 frames without either field remain readable.
Results are reconstructed from the journal; no second receipt file is authoritative.

Blobs are staged privately in the same directory, flushed, and published under
their SHA-256 names without replacing existing files. Existing blobs must have
the exact bytes and safe private-file attributes. The blob directory is synced
before appending. The complete candidate is checked against all replay limits,
including the cumulative historical blob budget. A valid incomplete tail may
be truncated to the checked committed boundary only by this locked writer.
It preserves the entire committed prefix. Reads and lookups never repair it.

The writer appends the checksummed event, syncs the journal and its directory,
then returns the new detached projection. A failed incomplete append publishes
nothing; a retry validates and removes only the torn tail before appending once.
After a complete append, sync or publication failure returns `commit_unconfirmed`
with a plain explanation: the change may be saved, so look up its operation ID
before retrying. It never claims rollback or appends a compensating event.
Lookup shows a complete recorded result without changing files. A matching
write retry syncs that existing journal and directory before confirming success;
it does not append again. A lookup alone does not certify a prior failed sync.

Before-append failures may leave unreferenced immutable blobs. A crash may
leave an unreferenced staging file. Neither grants authority or affects replay;
this component does not delete history or introduce automatic cleanup. Damaged
complete history refuses use and requires preserving the files for review.
Offline crash tests prove process-restart behavior; they do not simulate power
loss, hostile same-user writes or certify later native lifecycle policy.

## Native audit mirror (local, unpublished)

The committed goal journal is the authority for native changes. The audit log
is a delivery copy, never a replacement for that history. This internal
component adds no public goal operation, scheduler or agent launcher. The
portable Queue and the existing lane-control audit remain independent.

Every complete journal frame has a stable event ID: `native-goal-` followed
by the lowercase SHA-256 of the exact frame bytes, including its sequence.
An incomplete final frame produces no audit event. Before delivery, the mirror
validates the full committed journal and its private references. Invalid history
blocks native use even if an older audit receipt exists.

Under manager then shared control lock, the mirror confirms journal sync,
saves a private delivery intent, appends and syncs its audit row, then saves
and syncs a receipt. The audit file uses the existing nonblocking audit lock.
The separate mode-0600 files are `goal-audit-intent.json` and
`goal-audit-receipt.json`; they use the same rooted, no-follow and atomic private
I/O rules as the journal. Unsafe or corrupt delivery files fail closed.
They never replace or reconcile `lane-control-intent.json` or change a hold.

The read-only internal status shows the journal sequence, mirrored sequence,
pending event count and a plain explanation. It creates no files or repairs
intent. A mirror failure returns unavailable even if a receipt rename became
visible. A visible receipt alone does not prove a previously failed sync.
The final locked native admission predicate refuses pending delivery and
reconfirms the journal, audit, intent, receipt and directory sync before success.
Internal admission combines this predicate with the existing locked lane guard.
Native launch remains a later step; this component starts no work.

A receipt alone cannot show that earlier delivery rows still exist. Status and
admission inspect the complete audit file for every acknowledged event ID and
its strict safe row, matched against the committed journal. Missing or damaged
required rows make delivery unavailable, with a plain reason. Reconciliation
appends only missing required events and leaves all existing bytes, including
legacy and damaged rows, intact. It confirms delivery again before success.
Inspection is bounded to 64 MiB per pass and 16 KiB per line. Larger files,
oversized lines, unsafe files or incomplete final lines refuse native readiness;
they are preserved for review, never trimmed to make a check pass.

The adapter supplies `cli`, `web`, `mcp` or `native` as the mirror source.
It records the adapter that first prepared delivery, not an authenticated
owner identity or the original save time. Caller JSON, actor fields and spec
contents cannot select it. The intent retains that source and delivery time
across restart. Each row contains only the version, delivery time, fixed action
and result, event ID, sequence, goal ID, goal revision and source. It contains
no brief body, report, session, credential, operation ID, blob reference or
private path. Generic outcome auditing cannot emit this reserved native action.

If an older acknowledged row is lost after its intent was superseded, its
restored copy records the retry adapter and delivery time. The latest retained
intent still preserves its original adapter and time. This checks safe delivery
and journal identity; it does not authenticate hostile edits by the same user.

A crash after append and before receipt can deliver the same row again.
This is at-least-once delivery. Readers deduplicate valid native rows by event
ID and show their identity, source, sequence and revision with a plain summary.
External consumers must do the same; raw audit history is never rewritten.

Recognition and public decoding share an exact-key pre-pass over each row.
After JSON unescaping, known public and native field names must match their
lowercase schema names exactly; case variants, including Unicode case aliases,
are rejected. Repeated decoded top-level keys are rejected, including escaped
duplicates and mixed spellings such as `tool` plus `Tool`. Escapes that produce
an exact schema name are allowed. Readers recognize native rows from the
reserved action or top-level `native_audit_version`, `event_id` or
`goal_revision` fields, even when the action is null, empty or different.
The complete closed native schema must pass before any public entry or raw
text is built. Extra body fields, nulls and coerced values are rejected.
Genuine legacy rows with exact known field names and no native markers remain
readable, including unknown actions and repeated legacy rows. Removing all
native evidence is outside this recognition rule.
Older audit rows retain their existing classification and presentation.
An unfinished audit line blocks delivery for review; the mirror does not delete
it or pretend the event was delivered. Offline crash tests certify process
restart and reaping, not power-loss durability or hostile same-user edits.

## Native goal records (local, unpublished)

Internal manager operations create a drafted goal without coverage, replace its typed spec with
an expected-revision check, or append a note. These operations share the
existing manager then control-lock serialization and the same journal writer.
They introduce no command, MCP tool, HTTP route, approval operation, scheduler
or process launch. Portable Queue, configuration, reports, tails, Cancel and
lane holds do not load this journal as part of their existing operations.

Creation requires a new ID, revision zero and no seal. It grants no approval
or batch binding. Updates require the current revision and keep the ID fixed.
A changed execution scope or seal, including seal removal, returns the record
to drafted and clears approval and batch binding. Coverage then parks it or
closes it from an already verified target as described below. Execution scope includes
every spec field except title and presentation order. Changing only those
presentation fields retains the original binding; it does not approve the
new revision. Running and ended records refuse spec edits until their separate
lifecycle review rules are available. Verified and cancelled results are
permanent: preserve their spec, seal, state, outcome and bindings. A correction
adds evidence in a note or uses a new goal ID; it does not rewrite the result.

Notes remain in append order through edits and restarts. Each note has one
nonblank line of at most 1,024 UTF-8 bytes and an optional one-line evidence
reference of at most 128 bytes. Each goal retains at most 32 notes, with no
eviction. Encoded record metadata is limited to 65,536 bytes, including JSON
escaping. Reaching either the note-count ceiling or the encoded metadata
ceiling explains that capacity needs review. Encoded overflow has a distinct
reason from invalid fields, including when quote escaping fills the byte
budget before the note-count ceiling. It never removes
an earlier note to make room. Read DTOs redact note text and evidence and
return detached values; private seals and batch identities are never returned.

A version-1 journal event may now include paired `record_ref` and
`mutation_sha256` fields with its operation receipt. The reference is
`goal-record-` followed by the lowercase SHA-256 of the exact private metadata
bytes. It uses the same immutable staging, mode-0600, no-follow, single-link
and sync rules as spec blobs, and counts toward the cumulative replay budget.
Metadata is a closed object containing version 1, typed state, notes, and
optional outcome, approval revision and batch binding. Unknown, duplicate,
null, wrong-type, malformed or oversized fields fail replay. Historical
spec-only frames without coverage remain readable. Once record metadata exists, a spec-only
frame cannot erase it. Replay independently refuses lost notes and rewritten
permanent results, even if a frame's checksums are valid.

The storage fingerprint also binds the record reference and mutation digest.
The mutation digest binds canonical typed request JSON: operation kind,
operation ID, goal identity, expected revision and supplied spec/seal or note.
A repeated operation returns its original result even after later edits or
notes. Different input or a different operation kind refuses. Restart lookup
remains read-only and does not certify a previously failed sync; a matching
mutation retry confirms the existing event without appending another one.

Listings include every open record: drafted, approved, running, ended and
parked. Their open total is the length of that complete list. Verified and
cancelled records form a separate terminal total and page. IDs sort in ASCII
order. Terminal offsets are zero-based, and page size is 1–100. Each response
has one journal sequence for all rows, an explicit journal-presence flag and
an incomplete-final-append flag. If the sequence changes between pages, start
again rather than combine pages from different snapshots. Reads never repair
an incomplete tail or schedule work.

The entire encoded listing, including complete open rows, selected terminal
rows and totals, must fit 1,048,576 bytes. The exact boundary is accepted;
overflow returns a plain error and no partial list. Nothing is evicted from
the journal. An absent journal in a valid private directory is an empty list
with presence false. A missing directory has a separate missing-store error.
Unsafe roots, corrupt history and invalid references fail closed; they never
look like an empty queue. These component reads do not certify later native
approval, launch, lifecycle or public-adapter behavior.

## Native goal decisions (local, unpublished)

The internal decision operation records approve, verify, park or cancel with
the expected goal revision and a unique operation ID. Each decision needs one
nonblank line of reason, at most 512 UTF-8 bytes, and nonblank authority and
reviewed-evidence references, each at most 128 bytes. Control characters,
Unicode line and paragraph separators (categories Zl and Zp), invalid text
and irrelevant review fields refuse before writing. The same reason rule
applies when reading retained decision evidence. Saved
decisions remain in append order, with the revision they reviewed. Reads show
redacted reasons and references. Repeating an operation returns its original
receipt; changing its input refuses. The same private journal, locked save
protocol and uncertain-save lookup rules apply. History is never evicted.

Drafted goals may become approved, parked or cancelled. Approved goals may
become parked or cancelled. Ended goals may become verified, parked or
cancelled. Parked goals may become approved or cancelled. Verified and
cancelled results stay permanent; correction evidence is a note, and reopening
needs a new goal ID. Later lifecycle components own approved-to-running,
running-to-ended and explicitly authorized ended-to-running continuation.
Coverage holds and closure use the separate rule below. They are not decision
actions. A covered source cannot be verified by an independent verify action.

Approval needs a validated compiled seal for the exact stored spec, revision,
authority and current source observation. The internal compiler now supplies
this predicate; an opaque stored seal never grants approval. See
[configuration.md](configuration.md#native-brief-seals-local-unpublished) for
assembly, source review, size measurements and refresh rules. Brief delivery
is prepared separately; native dispatch remains a later component. Approval
starts no work.
An owner hold remains in force. A judge reference or adapter source does not
clear it. Approval grants no automatic batch and starts no work.

Candidate saves and authoritative history replay reapply the same decision
checks as the ordinary writer against the unchanged prior accounting.
Verification must name the current ended attempt and, for report review, its
retained digest. Alternate review still needs settled accounting, without an
active successor or an unconfirmed launch. Park and cancel obey the same busy
work refusal. Replay recomputes the same exact seal/source check for approval,
and refuses invented approval or batch binding. A seal change must remove the
old approval and grant, even in a lower-level save or replay. The same reset is
required when execution scope changes while the seal and decisions stay unchanged.
An unchanged decision history cannot approve a draft or parked goal, change its
approval revision or create an exact grant. Approval needs a new reviewed decision
with matching seal, authority and source observation. Append-only notes and title
or ordering edits preserve existing approval and old decisions. The resulting
metadata must match the writer's result exactly. A first history record cannot
contain decisions about a prior revision that does not exist. These checks
validate retained history; they do not authenticate a human reference or prevent
an unsandboxed process running as the same user from replacing all history.

Verification needs an ended current-attempt observation and explicit review.
Report review names that exact attempt and report digest. A report alone, a
previous attempt or a changed report cannot verify work. Explicit alternate
reviewed evidence can replace a report, including reviewed work without a run,
but it must still match the committed accounting observation. Missing
accounting, active successors and uncertain launches refuse verification.
This component publishes no attempt observation in production; later lifecycle
steps supply those observations. Test fixtures do not certify a real run.

Park and goal cancel are bookkeeping. They never signal or stop a process,
change a lane hold, freeze work, run recovery or schedule a launch. A running
goal refuses both. First use the existing separately authorized run-cancel
operation if needed, account for its actual end, then record the goal decision.
Its actual outcome remains unchanged, including when work completed before
cancellation. Existing report, tail, Cancel, Freeze and lane controls remain
independent of the native journal.

No CLI, HTTP or MCP goal surface is added here. Later MCP adapters retain the
owner host's Ask policy. Authority and evidence references record a human
decision; they do not authenticate an owner against an unsandboxed client
running as the same operating-system user. No source or role field changes
that limit or supplies an owner-hold release.

## Native dependencies and coverage (local, unpublished)

A prerequisite in `after` is satisfied only when that exact goal is verified.
Cancelled, missing, ended and parked work does not satisfy it. Failed work
still awaiting verification does not satisfy it either. This is
prerequisite truth only; it does not authorize admission or dispatch. All saved
goals participate in one graph containing both `after` and `covered_by` edges.
A missing target stays unresolved. Adding it later must still leave the complete
graph free of cycles, including mixed dependency and coverage cycles.

Structured coverage parks a source that has no running or unsettled attempt.
Running work, an active successor or an uncertain launch refuses coverage.
The source remains held while its target is unverified, including a cancelled,
failed or parked target. A target's explicit accepted review is the closure
authority. A verified target with that retained review closes its covered
sources, then their covered sources, in the same operation. This also applies
when coverage is added after the target was verified. A source's own unfinished
prerequisites do not prevent closure through the accepted target.

Each closed source shows `closure`: its immediate `target`, the exact
`target_revision`, and the acceptance `authority` and `evidence`. A transitive
closure keeps the same acceptance references. All keys use their exact spelling
after JSON unescaping. Case aliases, duplicate decoded keys, unknown fields,
null and wrong types fail. References and coverage reasons stay on one line,
including Unicode separators and U+0085. These references record the review;
they do not prove human identity against another unsandboxed same-user process.

Closure changes the source to verified and clears batch binding. It preserves
its actual attempt, outcome and notes. It never invents a run, report, duration
or independent verification decision. Permanent results and their provenance
remain unchanged; correction evidence still belongs in a note or new goal.
Dependents of a closed source now have a verified prerequisite. Repeating a
target verification returns its original receipt and emits no extra closure.

All affected revisions publish through one journal frame. Its optional
`changes_ref` binds an immutable private `goal-changes-` SHA-256 blob containing
the sorted complete change set. Each row names goal ID, revision, spec reference,
optional seal reference and record reference. The primary input stays in the
frame so both save and replay can recompute the same complete result. Missing,
extra, forged or partial changes fail before a projection is returned.

The cascade is bounded by the stored population limit of 4,096 goals. A change
set is at most 2,097,152 bytes and uses the existing safe blob I/O and cumulative
67,108,864-byte replay budget. The 4,096-byte frame ceiling is unchanged.
Capacity or unsafe-history failures refuse the whole operation; no subset
closes. A torn final frame exposes none of its changes. A complete frame with
an unconfirmed sync requires the existing receipt lookup and confirming retry.
No lane pause, process control, public goal adapter or scheduler is added.

## Native goal admission (local, unpublished)

Admission is an internal check. It starts no work, takes no repository claim,
and saves no reservation, prompt, run or goal event. It adds no dashboard, CLI
or MCP operation. The internal native launcher repeats this check and keeps the
same lock through reservation; an earlier eligible snapshot is not permission
to launch.

Ready means the goal is approved, every prerequisite is verified, the approved
brief seal still matches its execution scope, and no owner or coverage hold
remains. An opaque seal or initial bookkeeping without an approval decision
cannot make a goal Ready. Notes, title and ordering changes may retain approval.
The check reads the original approved spec and seal and uses the same strict
seal predicate as decision saving and history replay. A scope change revokes
readiness even when it retains the same seal. Escaped exact JSON keys are
accepted; case aliases, duplicate decoded keys and one-line references with
Unicode line or paragraph separators are refused.

Startable adds current source review, a configured allowed engine with an
explicit trusted capability for the work kind, and an eligible lane. Required
lane bindings never fall back. Pool removal or size zero takes effect on the
next locked configuration reload. Pause, draining, owner freeze, retained
messages, recovery work, running work and unavailable lifetime locks hold the
lane. A paused refusal keeps the existing primary lane_paused result and all
safe blockers. Each blocker also has a plain explanation. Resume starts no
work and never consumes a held message.

The manager mutex precedes the shared control lock. Inside that lock, admission
reloads configuration and reads the complete goal history and current controls.
It refuses incomplete journal tails, pending or damaged audit delivery, and
uncertain store durability. It does not repair history, resume intent or pause.

Trusted adapters supply known repository roots, lane capabilities, current
source observations and complete repository claim observations. Goal fields
cannot supply these facts. Write paths resolve through existing filesystem
ancestors to canonical paths inside their named repository. Whole-repository,
ancestor and descendant writes conflict; similarly named siblings do not.
Repository aliases cannot hide overlap and a link escaping the root refuses.
Resource names, including mint, are exclusive across reserved work.

Running native records and unsettled attempts retain reservations. An unsettled
attempt outside running holds new admission when its original scope is unknown.
Legacy running, held-message and recovery work needs trusted scope keyed by the
full run identity; a matching goal ID alone proves nothing. Unknown scope holds
new work rather than implying an empty reservation. A settled legacy run does
not reserve scope unless a message or recovery hold remains.

Claim observations must cover each required canonical repository exactly once,
be no more than thirty seconds old, and have no holder. Missing, duplicate,
future-dated or stale observations hold new work. This is a conservative check,
not an atomic transaction with Achta. The coding agent's repository claim is
still the final external guard; admission never force-releases another writer.

Goals sort by ascending order and then ASCII ID. Selection skips a blocked goal
and considers the next eligible one. Preferred lane wins when eligible, followed
by the preferred engine and ASCII lane name among allowed capable lanes. Ready
and Startable describe availability, not approval for automatic execution. No
batch grant or scheduler is enabled by this component.

## Dispatch an approved native goal (local, unpublished)

The internal dispatch operation takes the saved goal ID, its current revision,
a retry key and an eligible configured lane. The key uses one to sixty-four
ASCII letters, digits, dots or hyphens and starts with a letter or digit.
The closed request uses exact keys after unescaping. Replacement body or text,
unknown fields, case aliases, duplicate decoded keys, nulls and coerced revisions
refuse before effects. Trusted code supplies the source, lane capability,
current source observation, repository roots and delivery profile separately.
These references do not authenticate human consent. No public CLI, MCP or
dashboard goal-dispatch operation or automatic batch is added.

The requested current revision is separate from the reviewed sealed revision.
Notes and presentation edits can advance the former without replacing approved
bytes. A new request must match the current revision. An exact retry resolves
its saved receipt first, even after a later note. Changing the goal, lane,
revision, selected profile or adapter source under that key refuses.
Internal native requests serialize on the existing control inode, so concurrent
exact retries read the same confirmed outcome. Public lane controls retain
their existing nonblocking contention refusals.

Dispatch holds the manager mutex and existing shared control lock through final
admission, reservation and launch. It applies the same admission predicate to
the requested lane. Required lane binding still cannot fall back. Stored native
IDs cannot use legacy free text, reply, held-message delivery, continuation or
recovery to bypass this accounting. Unrelated legacy goals keep their argv and
supervision.

The journal stores a permanent receipt in the record's closed `dispatches`
array. It binds request, original approved spec and seal, delivery descriptor,
canonical write paths, full run ID, adapter source and launch phase. Resource
names remain in the approved spec. Goal-wide and lane/write/resource holds
survive cooperating managers and restart. Writer and replay share the same
retained-phase predicate, including transitions that append no decision or
keep the same seal. No scope edit can release unsettled work.

The public goal stays approved while the internal phase is reserved or starting.
The reservation and its audited intent sync before private run preparation.
The same full ID names the pending directory, prompt and metadata, then the
published run directory. The exact retained delivery is checked again before
the start intent. Starting syncs before any possible OS invocation. A running
receipt binds the exact PID, kernel start identity and job evidence, the current
attempt and running state before success. Native audit events distinguish
reservation, start intent, running and proven nonlaunch; stable event identity
deduplicates redelivery. No private prompt or filesystem path enters that mirror.

Failure before start intent can publish a durable failed receipt, leave the
goal approved and release its resources. A repeated key returns that same
failure; a reviewed later dispatch needs a new key and current revision.
Once start is possible, errors, missing metadata or one observation of an
absent PID do not prove nonlaunch. The full ID and resources stay held.
Retained unstarted preparation is held for review rather than launched again.
An exact identified live child can be supervised and published once on retry.
Unknown or mismatched identity is never signalled or replaced.

Visible complete journal/audit bytes after failed sync do not mean success.
Retry confirms durability before publishing the same result. A torn final
frame remains nonvisible and holds native dispatch. Readers never repair it.
Receipts are limited to eight per goal and the existing record, event and byte
limits still apply; there is no eviction or forgotten retry key. Publication
headroom is checked before starting. Capacity refusal requires owner review.

B11 does not automatically account for native ends or permit successors.
An immediate child exit stays bound and conservatively reserved; a report
cannot verify the goal. B12 and B13 own end and successor integration.
Physical power-loss durability, real lane sandbox visibility and atomic
external repository claims are outside these offline proofs.
