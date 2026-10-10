# OpenCode event fixture provenance

`opencode-events.jsonl` is synthetic, based on the upstream run emitter and
tool-part schema reviewed on 2026-10-10:
https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/cli/cmd/run.ts

The installed CLI reports 1.18.35. An isolated Step 5 Preview Free attempt
returned exit 1 with one error event, a numeric timestamp and sessionID present;
no successful model output was captured. These fixtures remain unverified
against real model output. Fixture timestamps, IDs and paths
are invented. No credential or tool output is copied from a real session.
`must-not-return` proves result/output exclusion. Unknown-event rows exercise
forward compatibility rather than asserting a native event type exists.

Upstream emits tool_use with part.tool, part.callID, state.input and status
completed/error; text has part.text. Steps and errors share millisecond timestamp
and sessionID. The offline fake also supplies a synthetic running tool event
to test timing tolerance; current upstream does not emit that start event.
