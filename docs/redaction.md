# Source privacy contract (v0.3.6 slice 1)

Policy ID: source-pass-2. Q1 covers recognized private/uncertain contexts and
contributing pending ranges. Marker-free key recognition is not promised.
Any orphan, unmatched, nested, mismatched or out-of-order marker hides its
whole segment/event. Later markers never release uncertainty. Earlier fully
approved unrelated events survive unchanged.

## Source-to-sink census

This inventory covers the source entrances and public reader families in main
a7c5455. Go discovery uses the precise graph and callers of Redact, bounded,
readWindow, eventWindow, Report and safeJSON. A family is one shared reader,
not a count of HTTP routes. Slice 2 below is an explicit proof gap.

| Source | Readers / public sinks | Profile / transformation | Cache | Proof slice |
| --- | --- | --- | --- | --- |
| Complete report file | Manager.Report; data history/report/overview; MCP report; menu report | ordinary; approved clipping | reportCache of approved report | 1 |
| Original event file | eventWindow; Tail; data tails/activity; currentAction/status/writeStatus; terminalTurn | ordinary, terminal for stopped final; summaries after projection | digest-bound eventWindows and approved line entries | 1 |
| MCP result/error object | safeJSON/safeValue; toolText; content and full response; data serialization | ordinary; original keys and values before output | no raw display cache | 1 |
| goal.txt, oversight file | readDisplay; data history goal/oversight | complete plain source before clipping | approved data response | 1 |
| Owner note/freeze state | AddOwnerNote/readNotes/freezeState/Freeze; data/MCP | Redact/bounded | persisted redacted notes/state | 2 |
| Held message file | webHeldMessages/laneCommand; held review HTTP/CLI | Redact/bounded before view | review DTO | 2 |
| Native goal metadata/journal | nativeGoalView/goalRecordView; title/hold/coverage/notes/decision/closure views | per-segment Redact | native DTO | 2 |
| Run metadata/configuration | identityValue/cell/runOutcome/settingsView; status/history/menu/CLI human/JSON | Redact/bounded | typed views | 2 |
| Audit journal | parseAudit/Manager.Audit/appendAudit; data/CLI audit | Redact/bounded | auditCache | 2 |
| CLI / background failures | CLI/serveCLI/webCommand | Redact on error strings | none | 2 |
| Notification / signals | endRun/emitSignal; names and typed state | structured names, no raw event bodies | signal files | 2 |
| Browser DOM/AX | dashboard consumes data result | approved DTO; separate browser proof | browser state | 2 / parked environment |

Control parsing (session, continuation admission, stop, native journals) remains
private. It cannot grant display approval. No new permission or launch behavior.
Slice 2 retains existing redaction for its sinks; no raw-display exemption exists.

## Grammar and exact budgets

| Rule | Exact contract |
| --- | --- |
| Normalization | Unicode uppercase; remove unicode.IsSpace; ASCII '-' only; rendering retains original bytes |
| Complete delimiter | five ASCII dashes, BEGIN/END, optional RSA/EC/DSA/OPENSSH/ENCRYPTED, PRIVATE KEY, five dashes; case/space tolerant |
| Uncertain intent | existing privateFragment language; unknown complete private labels uncertain; bare trailing '-' contributes pending |
| Roles | OpenCode text/part.text; Claude assistant/message.content ordered text elements; Codex item agent_message/text; typed member/index path and matching session |
| Other keys/values | inspected in wire order; private intent latches whole event even if balanced |
| Structural literals | only exact schema paths and fixed validated type values survive OPEN; unrelated type/session/metadata values are hidden; the finite fixed envelope key names are exempt at any path |
| Opaque scalars | numbers, booleans and null count toward the descriptor cap; inside OPEN they hide the whole held range rather than render unproved data |
| JSON | UTF-8 and paired surrogates required; decoded duplicate keys, malformed structure/escapes rejected |
| Physical event | 1 MiB maximum, LF framed; CRLF retained; polling EOF cannot seal |
| Source scan | 64 MiB maximum; start at source zero; no inferred window context |
| Plain display input | existing 65,536 report bytes; oversized complete segment hides rather than approving a clipped prefix |
| Decoder | depth 64; 8,192 scalar descriptors per event |
| Spans | 8,192 markers and 8,192 replacement descriptors per scope; excess hides |
| Held range | 1 MiB original bytes; exhaustion hides from first contributor and latches suffix |
| Public event cache | existing 64 windows, 8,192 approved line entries, last 200 Tail summaries |
| Lexical reuse | at most 8,192 exact approved complete scopes and 1 MiB per window; only in CLEAR under identical policy |
| Output | report 64 KiB; final 8 KiB; Tail 200 rows/60,000 aggregate bytes; action 120 runes |
| Filters | secretPatterns and terminalPrivateText declarations unchanged; terminal profile adds restrictions |

Only sealed projection values leave the pass. JSON replacements splice full
original escape/rune intervals; untouched escapes/order/whitespace stay exact.
Source reconstruction and digest comparison precede cache reuse; same-size
rewrite, append, rotation and eviction never authorize a raw body-line hash.

String-valued `item.arguments` on an `mcp_tool_call` requires a second JSON
document decode without a proved original-coordinate mapping. The shared pass
hides that event and the generation suffix, preserving earlier approved events.
This applies even to public or empty strings. Object arguments retain their
normal inspection and exact approved wire. Standalone summary callers obtain
string arguments through `approvedSourceArguments`, which approves the entire
bounded document before decoding approved object values or selecting fields.
The summary adapter never performs its own nested JSON decode. The new policy
ID invalidates older display-cache decisions.

## Slice-2 review items (R2)

The review census records inherited boundaries still owed by slice 2. CLI lane
pause/resume (`cli.go`) and native notifications (`manager.go`) display lane
names directly. `signals.go` constructs structured names from raw lane/goal
identifiers. Lane-control responses (`web_lane_control.go` and
`projectLaneControls` in `lane_control.go`) retain raw Name fields. Internal
control identifiers must stay intact; their display names and response DTOs
need separately approved projections and entrance tests.

Audit viewing still uses `readWindow` in `Manager.Audit` and independently
approves selected fields/raw strings in `parseAudit`. Slice 2 must establish
one ordered source decision before audit field selection and prove the actual
audit entrances. No runtime lane-name or audit disclosure is claimed by the
slice-1 repair. Owner-access presentation and private journals stay separate.

## Permanent proof and retained limits

`make redaction-proof` runs deterministic regression/property/mutation checks,
then each named fuzz campaign for 60 seconds with two workers. `make verify`
includes the same recipe. Mutations use confined temporary Go overlays, keep
original source byte-identical, require semantic test failure, then re-run the
affected GREEN predicate against restored source. Neither compilation failure
nor a missing mutation site counts as a killed mutant.

Ported review fixtures retain F1–F3, J01–J05, PM01, AF01–AF06, FS01, JP00–JP07,
TP00–TP03, CP01/CP02 and W01–W10. The old P03 expectation is replaced with
whole-segment hiding under the owner rule. W10 uses the new display snapshot,
keeping earlier complete events exact and unfinished event contents hidden.
Original archived files remain unchanged. The hotfix-only unknown-age feature
test is outside this main-based slice; native main age controls remain unchanged.

Permanent independent-review regressions cover nested MCP arguments, Unicode
separators/escaped and duplicate keys, and primed/reversed cache schedules.
M14 bypasses the shared nested-argument guard and must fail the nested probe
semantically; compilation failure does not count. Full acceptance and commit
remain dependent on the separate marker and lifecycle race fixes (judge QH).

The plain and event references use their own normalization, token traversal,
path types and span/state code. Exact output, scope/frontier and original
coordinate comparisons run for all directed byte prefixes and in topology,
prefix and cache fuzz. Private keys are generated after recipe decoding and
never saved in fuzz corpora. Ed25519, RSA and EC fixtures have parse/sign/verify
controls. DSA/OpenSSH/encrypted labels have grammar controls; parsed validity
for those envelopes remains a slice-2 proof item.

This implementation reconstructs a bounded whole snapshot on append, then
shares the digest-bound approved cache result. Exact sealed lexical facts may
avoid repeat parsing only in CLEAR. Open, pending and uncertain ranges cannot
use them. Format caches reuse only this version's approved projection. Cold
reconstruction stays authoritative. Incremental checkpoint reuse,
all-sink recipe expansion, broader topology generators and DOM/AX proof remain
slice 2. These limits must accompany source review; finite campaigns do not
certify arbitrary inputs or marker-free key recognition.
