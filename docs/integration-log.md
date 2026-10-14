# Integration log v1 (internal, not enabled)

This component records integration intent separately from native goals. It
adds no command, MCP/HTTP operation, queue, scheduler, executor, runner, Git
operation or policy setter. Submission records work; it does not accept it.
The later goal bridge owns production callers. Existing launches never read
this log. An unsupported or damaged integration store affects integration only.
There is no private class in this ordinary store; a separate instance and store
must own any later private workflow.

## Storage and independent bounds

The caller selects an existing, owner-owned 0700 directory. Rooted no-follow
opens refuse linked path components. `integration-journal.bin` and the dedicated
`integration.lock` are owner-owned regular 0600 files with exactly one link.
Missing journal means empty integration history; reads create nothing. Writes
create only those integration files. Neither `goal-journal.bin` nor lane-control
state receives new frames, kinds, revisions or capabilities.

Each frame has an 84-byte header: ASCII NIJF, big-endian uint32 version 1,
uint64 sequence, uint32 payload length, SHA-256 of the first 20 header bytes,
then SHA-256 of the payload. Sequences start at 1 and increase by one. Closed
JSON payloads bind the previous payload digest and the canonical request digest.
The independent capabilities allow 16 MiB journal bytes, 8192 complete events,
1024 distinct entries and 32768 bytes per payload. Exact limits are accepted;
overflow refuses without deleting history. Invalid headers, unsupported versions,
corruption, duplicate keys, unknown/aliased keys, omitted required fields, null,
wrong types and any truncated final frame refuse the whole projection. Readers
and writers do not truncate, repair, compact or discard a tail.

## Entry identity and review

An entry binds an existing goal and review goal, their current revisions and
exact spec/seal/record digests. The review goal must have review work kind;
a seal additionally requires a verified review record. Goal identity uses the
native dispatch grammar. Entry, operation, repository and semantic IDs use
1–64 ASCII letters, digits, dots or hyphens, starting with a letter or digit.
Git object identities are lowercase 40- or 64-digit hexadecimal strings.
All other digests are lowercase SHA-256. Policy epoch and attempt are positive.

The entry includes source commit, base commit, tree, operation/fragment/reviewer
digests, repository ID, group digest, canonical relative scope, semantic keys,
policy digest/epoch, profile digest, authority digest, predecessor and dependencies.
Scope and semantic/dependency arrays are explicit, unique and bounded to 32.
They describe immutable inputs; no hash or caller-supplied authority field grants
OS access or proves owner approval. Scope syntax alone does not resolve a repo.

The closed operations are submit, revise, seal, intent, observe and hold. CAS
uses the entry revision; each saved event increments that revision and the log
sequence. Global operation IDs bind canonical typed input. The same operation
with identical input returns its original journal identity after durability
confirmation, without another append. Different input with that ID refuses.
Another submit cannot replace an entry. Revise clears every seal, even for
unchanged bytes, and cannot change goal/repository/attempt/dependency lineage.
Rejected entries remain terminal. A new attempt references an existing parked
or rejected predecessor of the same goal/repository, with no unresolved effect,
and increments its attempt ordinal. Missing/self dependencies refuse; retained
dependencies remain ineligible until a later authenticated acceptance component
can establish them. There is no acceptance operation in this version.

A review seal binds the complete entry, review and reviewer digests. Submitted
work is ineligible. Reviewed work without dependencies or pending effects is
structurally eligible. This internal predicate is not a live execution permit:
new work revalidates source/review state, and the later bridge must perform
its own fresh policy, authority, resource and source checks before effects.

## Durable intent, observation and locks

Capture native source/review state under control.lock and release it. Then take
the independent nonblocking integration lock, replay, check CAS and validate the
complete candidate. Append, fsync the journal and fsync its directory before
acknowledging. Release integration.lock and recheck source/review revisions under
control.lock. These locks are never held together. Real effects, launch and long
gates have no callback or implementation here. The internal IO fault seam exists
only for deterministic storage tests and is not a runner interface.

The intent vocabulary is prepare, gate and promote; recording one performs none
of them. An unresolved intent prevents another effect and cannot become success
on restart. An observation binds the exact intent and result digest and says
settled, failed or uncertain. Uncertain retains operation ownership and state;
only a later exact observation may settle it. Failed parks the entry. Settlement
permits further reviewed work but never means acceptance, publication or closure.
Holds retain parked, rejected or uncertain state and attempt history.

An observation of an existing pending intent, or an uncertain hold of that
intent, does not require the source or review to remain unchanged. Locked replay
still checks entry CAS and exact observation binding. Exact result retries remain
confirmable after settlement. This exception records started work; it grants no
new effect or seal. Those operations still require fresh source/review state.

Full stores, stale CAS or source snapshots and duplicate/corrupt operations refuse.
Partial append, fsync failure, directory-fsync failure, lost acknowledgement or a
source change after durable append returns unconfirmed, never effect success or
rollback. Replay retains complete intent; a torn tail holds all integration.
An identical retry confirms durability of a complete save only. It cannot run
an effect or supply a missing observation. Preserve failed history for review.

Checksums, seals and stored operation identities are untrusted against coherent
same-user replacement or rollback. This component supplies no protected receipt
authentication, retained tip, live lease or key custody. Later custody and anchoring
must land before any acceptance claim. Offline fsync fault tests do not certify
physical power-loss durability or host sandbox isolation.

## Offline proofs

`TestIntegration*` uses existing native goal fixtures, private temporary roots,
compiled fake agents and compiled test children. It checks CAS and duplicate
identity, stale goal/review snapshots, seal invalidation, missing predecessor,
dependency holds, uncertain intent settlement, acknowledgement/fsync failures,
strict replay, capacity, private-file safety and two-controller/process contention.
Pending-result tests change both goal and review revisions, retain terminal and
uncertain results, settle uncertainty later, and retain stale-source guards.
The source-lock test changes a native revision after snapshot while independently
acquiring the integration lock; final acknowledgement refuses the stale snapshot.

The legacy launch control uses the test suite's compiled CLI. For the dispatch
downgrade proof it also accepts `NUNTIUS_INTEGRATION_BASE_BINARY`, pointing at a
binary built from the exact dispatch baseline. Both valid and unsupported
integration frames must leave ordinary fake-agent launches working. No real
agent, credential, DNS/network access, owner store or installed service is used.
The native goal journal's bytes are checked unchanged across integration writes.
