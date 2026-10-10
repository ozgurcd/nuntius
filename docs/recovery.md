## Away from the Mac

Use the Claude mobile app to tell the judge to check status, answer questions,
freeze work, or send a reply. The Mac runs nuntius; mobile reaches it through
the judge and Claude Desktop, not through a public dashboard port.

The Mac's own energy settings decide sleep. Nuntius installs no energy helper. The menu-bar helper can start at login
only when you opt in with `nuntius menubar --at-login on`. Claude Desktop must be open for the judge to reach MCP.

## Crash and restart safety

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
