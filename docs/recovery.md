## Away from the Mac

Use the Claude mobile app to tell the judge to check status, answer questions,
freeze work, or send a reply. The Mac runs nuntius; mobile reaches it through
the judge and Claude Desktop, not through a public dashboard port.

The Mac's own energy settings decide sleep. Nuntius installs no energy helper. The menu-bar helper can start at login
only when you opt in with `nuntius menubar --at-login on`. Claude Desktop must be open for the judge to reach MCP.

## Crash and restart safety

### Native partial launches (local, unpublished)

Internal native dispatch reserves the full run ID before preparation and records
that start may occur before invoking the OS. A lost response does not authorize
another child. Retrying the exact goal/revision/key confirms the saved receipt.
An identified live child must match the full ID, goal, lane, approved prompt
digest and kernel process identity before adoption. Missing, partial or mismatched
identity keeps the launch held for review; no guessed PID is signalled.

An unstarted preparation can keep a review hold. A confirmed failure before any
start intent leaves the goal approved and records a permanent failure receipt.
Use a new key and current revision only after reviewing that proven nonlaunch.
Once invocation is possible, an error or an absent process is insufficient
evidence to release the reservation. Journal and audit sync failures retain the
same operation. Ordinary legacy recovery cannot launch a replacement native ID.

Native automatic end accounting repairs the original full run identity,
including an immediate exit before running publication. Internal linked
successors use the continuation contract below.
Cancel, Freeze, report and tail remain separate safety/read operations. Lane
pause changes no timeout or signal policy. Offline killed-caller tests prove
conservative adoption or hold; they do not certify physical power loss or an
installed host.

### Native end repair (local, unpublished)

Writable startup and maintenance reconcile actual native ends without starting
replacement work. The saved run end is the retry source if its native journal
append or audit delivery fails. An already-ended run is included in repair;
read-only status and replay leave files unchanged. A complete frame after a
failed sync must be confirmed before its original receipt is accepted. An
incomplete final frame is repaired only by a locked writer. Corrupt history
remains held for review rather than rewritten.

The last settled attempt becomes ended, still open for judge review. Missing
reports, timeout, cancellation and terminal report-delivery failure keep their
typed outcomes; numeric exit is never guessed. A report seen later may add its
bounded full-file digest to the current attempt without changing that outcome
or verifying work. Held messages, recovery pending, successor links, surviving
unverified process identity and failed delivery keep work reserved. Lane pause
does not stop or extend the current run; its end is recorded while paused.
Legacy free-text dispatch cannot replace a stored native goal.

An eligible native interruption now records `recovery_pending` before its end
can release resources. It remains ended and open while paused or awaiting fresh
trusted admission context. Its recovery count does not change. An eligible
Claude acknowledgement or accepted owner text likewise retains a named pending
hold; accepted text has priority. Ordinary and alternate judge verification both
refuse pending work, even when a report arrives later. Writable repair preserves
the first observation and passive reads change no files. Resume starts nothing.
An in-process trusted continuation resolver must supply fresh source,
capability, repository-root and claim observations at selection and immediately
before OS invocation. Each observation must agree with the retained approval,
delivery profile and canonical inode evidence. Stored digests alone cannot
satisfy this contract. A reopened manager needs the resolver again; there is
no new public native adapter or configuration switch.

Accepted text, eligible Claude acknowledgement, normal interrupted recovery,
explicit ended Reply and exact held Send use the same linked accounting.
One durable full child ID and input/session digests bind each parent before
start. The child retains the original approved brief and scope separately from
its private execution input. A proven chain owns one reservation; historical
ends and reports cannot release its current child. Uncertain preparation or
invocation retains that same intent for adoption or review and never resends.
Exact held Discard appends settlement only when no child is possible; an
already-started or uncertain child stays reserved. Resume sends no held text.
Automatic recovery starts at most twice, uses the original brief and honest
unknown-exit note, and does not spend a count while paused or refused admission.
Writable status repairs/adopts existing work but starts no new successor.

A failed native repair does not prevent manager startup, report access or
identity-checked cancellation. Writable status carries a plain `native_attention`
warning while repair remains pending. Native admission still checks authoritative
history and required audit delivery and refuses damaged authority. Stores without
a native journal or actual run require no end-reconciliation scan.

### Legacy runs

Every accepted goal is published durably before an agent starts. Its private
record contains the lane, goal ID, original brief, session ID once detected,
resume count and links to earlier/later runs. On macOS, each agent starts through
a transient `gui/<uid>/nuntius.run.<run_id>` launchd job. Its private plist lives
only in that run's data directory, never in `~/Library/LaunchAgents`. Bootstrap
loads it without `RunAtLoad` or `KeepAlive`, then explicitly starts it once.
`LaunchOnlyOnce` allows launchd to discard the finished job even while MCP is
down. An exec-only bootstrap becomes the agent; no wrapper, daemon, login item
or watchdog remains. Standard input, output and error use the run's files.
The bootstrap restores the same argv, working directory and inherited environment;
environment values cross a bounded private pipe and are never saved to disk.
The launchd plist carries the dispatching nuntius process's inherited umask,
captured at startup, with 022 as the fallback. Starting nuntius from a Terminal
with umask 022 therefore gives its jobs 022, rather than a forced 077.
Private run files still use 0600 and directories use 0700.
Linux keeps the double-fork/session start used in v0.2.3.

- App quit/restart or MCP crash: the macOS agent belongs to its own GUI job,
  outside the app's job. A live nuntius process polls its PID and kernel start
  identity and removes the recorded job on end, cancellation or timeout.
  Recovery retries stale-job cleanup using only labels in intact run records.
- A launchctl failure uses the v0.2.3 detached start. A fixed `launchd_fallback`
  event and `launch_fallback` status flag warn that app-quit survival is unavailable.
  If a partial start cannot be safely identified and removed, the recorded goal
  stays pending rather than starting a duplicate or killing an unknown process.
  Before any start is attempted, an unavailable launchctl still permits fallback;
  an unconfirmed inactive job label remains recorded for cleanup retries.
- Nuntius upgrade: replacing/removing the nuntius executable does not affect
  an already running agent. Nuntius does not reinstall or restart owner apps.
- Reboot, power loss or agent crash: the process can end, but the durable goal
  remains. Recovery runs when the app next starts its MCP server, or explicitly:

```sh
nuntius recover
```

Recovery marks an unreported, unstopped end without a confirmed terminal turn `interrupted` and emits
`<lane>.<goal>.interrupted`. With `auto_resume` enabled, it resumes the agent's
own recorded session, or starts the same brief fresh if no session ID is known.
It prepends a neutral note: the previous run ended before report delivery,
and its exit cause is unknown. The agent must re-measure previous work.
Repeating part of the work is acceptable; no periodic work journal is required.
Each goal chain permits at most two automatic recovery attempts. Each attempt
emits `<lane>.<goal>.resumed`; interrupted goals beyond the limit stay visible
for the owner. History links the chain and displays `Ended · resumed 1/2`
(or the failed, cancelled or timed-out outcome with the same count).

A busy lane leaves the interrupted goal `waiting to resume`. The running MCP
server retries when the lane frees. Freeze prevents launching; cancellation
and timeout never trigger recovery. Disabled or removed lanes retain their
interrupted records. `status`, `agents`, `history` and web views only display
and observe; they never launch recovery. Without MCP, run `recover` again after
a busy lane frees. Recovery is a local action explicitly enabled by the config.

A lane pause closes admission but lets the current process finish under its
existing timeout. It also blocks automatic continuation and new recovery
children. The interrupted goal remains reserved, with its attempt count intact.
Use `nuntius lane list` to see the plain reason. `lane resume NAME` clears only
that lane's hold; it starts no work and does not unfreeze. A later explicit
recovery check or the existing server recovery policy can then consider it.
CLI list/status/agents do not initialize files, repair history, reconcile
control intent or adopt a supervisor. If control storage is unavailable, they
show that uncertainty separately from the current run and report.

An owner message accepted before pause stays held for review, even after resume
or restart. It is not automatically replayed. Use `lane held review NAME RUN`,
then explicitly send or discard its content revision and digest as described in
[cli.md](cli.md). A missing original session or changed agent kind refuses send;
discard remains available. A delivery timeout or uncertain response is not proof
that a child started or that the message was consumed.

End detection derives `completed` from a regular report, otherwise
`ended_no_report`. Exit codes are unknown because no parent waits for the agent.
For OpenCode, a complete event stream whose last step ends with reason `stop`
confirms that the model turn ended. Without a current report file, a new run's
outcome is `report_delivery_failed`, status is `ended_no_report`, and the label
is `Ended · report missing` with an amber attention badge. This is incomplete
report delivery, not successful completion. No automatic recovery child or
interrupted signal is created. The judge can read the bounded, redacted final
assistant text through the tail and choose whether to reply manually. Nuntius
does not create a report from that text or relax any agent write guard.

Terminal observation reads the complete private stream, independently of the
tail window, with at most 64 MiB per stream and 1 MiB per newline-terminated
event. Malformed, truncated, oversized, unknown or unbalanced events, a later
step start, and top-level errors cannot prove a terminal turn. These retain
the existing conservative recovery policy. A missing session ID does not
invalidate a complete turn; it still limits manual same-session reply. Existing
ended records are observed before recovery without rewriting their history.
A terminal event while the verified process is alive leaves the lane busy
and runtime supervision active. Current regular reports still complete;
stale or linked files do not. Cancellation, freeze and timeout take precedence.

No process is signalled unless its PID, start time and process group match.
Job removal also checks any live job PID against that identity; unrelated or
unrecorded labels are never removed. Cleanup failure is audited and retried.
When no nuntius process runs, only the agents run; end detection, timeout and
recovery wait until a nuntius process next checks the store.

Metadata, config and signals use temporary files, file fsync, atomic rename
and directory fsync. Run records carry an integrity hash. Torn/malformed records
and incomplete temporary files are ignored while intact records remain usable;
linked files and I/O failures still refuse unsafe access. Invalid configuration
leaves MCP non-serving. This cannot repair damaged storage or stop an external
program from explicitly killing an agent. Offline proofs cover parent loss,
MCP death, binary replacement, recovery policy, waiting and torn files; real
Claude-app restarts and reboots still need the owner's pilot.

Tool approvals in Claude Desktop:

- **Ask**: `nuntius_dispatch`, `nuntius_reply`, `nuntius_cancel`, `nuntius_unfreeze`.
- **Allow**: `nuntius_status`, `nuntius_history`, `nuntius_tail`, `nuntius_report`,
  `nuntius_owner_notes`, `nuntius_owner_notes_ack`, `nuntius_freeze`.
  `nuntius_signals` and `nuntius_signals_ack` read and acknowledge end signals.

Freeze is immediate: it persists a marker, interrupts verified running runs,
and refuses dispatch, reply and automatic continuation until unfreeze.
Unfreeze through the judge requires the owner's tap in the host. The local
dashboard and `nuntius unfreeze` also unfreeze. Status returns an object with
`frozen`, `freeze` (time and redacted reason) and `lanes`, including each
running lane's `last_event_age_seconds` to spot a hung run. Freeze still shows
when both pools are disabled. Owner notes are append-only in `notes.jsonl`;
the judge reads at most 50 after `since` (RFC3339) and acknowledges their IDs.
