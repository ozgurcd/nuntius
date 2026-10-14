# Passive native goal projection

B14 adds the internal `Manager.projectGoals(goalAdmissionEnvironment)` read.
It consumes the B01–B13 native journal, records, approved seals, verified-only
dependencies and retained reservation accounting, plus A's lane-control DTO.
It adds no CLI, MCP, HTTP, scheduler, grant writer or workspace tracker adapter.
These adapters can consume the snapshot in later goals. Existing status
maintenance keeps its metadata-only background path and cache bounds.

## Snapshot identity and counts

The version-1 snapshot captures the journal sequence, lane-control revision,
run observations, source/capability/workspace/claim observations, consistency,
attention and the complete bounded open population. Manager and shared
read-control locks protect capture; calculation and JSON encoding happen after
both locks are released. No native record is repaired or committed by a read.

`revision` is an observation digest, not a CAS revision. It hashes the safe
snapshot before populating its digest and lane-status digest copies. Its inputs
include the journal sequence, control revision and observation timestamps.
Consecutive reads can therefore have different observation digests without a
goal mutation. Consumers retain each whole snapshot and never join counts or
lane rows across digests. Mutation adapters still use goal/control CAS rules.

`Open` is exactly drafted + approved + running + ended + parked. Each ID appears
once, ordered by native order then ID. Verified and cancelled are separate
terminal totals, not open rows. Ready is the approved, sealed, nonrunning,
nonreserved subset without owner/coverage/unverified-dependency holds.
Startable is the Ready subset passing fresh source/claims, physical scope,
audit visibility, reservation conflicts and an available matching lane.
NeedsOwner overlaps the open states; it is not another partition cell.
Snapshot overflow is an error, never a truncated successful population.

## Lane reasons and observations

Each configured or retained lane has the original lane-control view, passive
run state/outcome, all matching open candidates and their blockers. Required
lane bindings and engine/work-kind compatibility remain distinct. A blocked
high-priority candidate does not hide lower startable work. Manual-ready work
requires attention and cannot justify a quiet stop. Running, pause, freeze,
unavailable control, retained messages and recovery remain lane-level holds.
No idle TTL or elapsed-idle excuse is inferred.

Draining lanes keep their current run outcome and reservations. Unknown legacy
scope, unbound native running records and unmatched retained native dispatches
make consistency uncertain. Ended records need review; reports never verify
goals. Known source changes or occupied claims are blockers; unavailable,
partial, duplicate, future or older-than-30-second claims are uncertain.

Observations use fixed provenance labels and declared repository aliases or
goal IDs. Physical root paths, source strings, claim-holder text, error bodies,
briefs and session data are not returned. One complete claim callback result
serves the entire snapshot. Source timestamps record the trusted callback
invocation, not a filesystem or Git freshness certification. Callbacks are
trusted in-process read observers using the allowlisted admission environment;
this read never executes Git, fetches, dispatches or rewrites workspace files.

Audit visibility is checked without the admission durability-sync helper.
Consequently a Startable observation is not final launch authority: dispatch
must repeat the established B10/B11 admission and durability checks. Read-only
manager instances may observe manual startability but cannot dispatch.

B15 grant authority is not available in this base. `automation_available` is
false and `automation_reason` is `grant_unavailable`; neither approval nor a
stored grant-binding string supplies automation authority. B14 consumes no
future grant interface and exposes no automatic launch path.

Lane `Status` carries optional `computed_idle` and `goal_projection_revision`
fields only when supplied by this projection. `writeStatus` can render an
explicit `native_queue` snapshot and its lane-status rows. Ordinary public
status adapters do not invoke the new scan; later adapter goals wire that
surface using the whole snapshot. Run outcome fields retain their meanings.

## Offline verification

`goal_projection_test.go` covers the state partition/ID sets, Ready/Startable
subsets, owner overlap, lower-candidate manual attention, draining conflicts,
uncertain claims/source observations, shared claim reads, consistency holds,
detached snapshots, composite identity, response bounds, text DTO integration
and byte/mode/mtime preservation of the private fixture store. Fixtures use
temporary directories and no real agent, owner store or network.
