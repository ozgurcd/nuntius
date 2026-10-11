# Dashboard security notes

## Lane controls through judge MCP

Pause blocks new starts immediately and lets an existing run finish under its
normal supervision. Resume clears only that saved hold; it starts no work and
does not deliver held messages. Status keeps run failures, reports and hold
reasons visible separately. Corrupt controls block starts while status,
reports and independent Cancel/Freeze tools remain usable if their own state
is healthy. Configuration and run-store startup faults still fail closed.

Keep both `nuntius_lane_pause` and `nuntius_lane_resume` set to Ask for every
invocation in the owner's judge MCP host. Resume can enable future execution;
pause changes availability. Neither is autonomous judge authority. The server
has no trustworthy caller identity or approval attestation. It rejects supplied
roles and sources; the adapter supplies the audit source `mcp`. Annotations are
descriptive, not authentication. Same-user file modes do not prevent an
unsandboxed client with store access from launching its own stdio server.
Do not register coding agents, relax sandbox or hook rules, or write host settings.

Each request requires the current bounded integer `expected_revision`. Stale
requests fail even when the desired hold already matches; fresh repeats are
unchanged. Closed, duplicate-free arguments are bounded to 4 KiB, with no nulls
or number/boolean coercion. Only pause accepts strict boolean `after_current`.
The cooperating nonblocking locks and manager audit protocol remain shared
with CLI controls. The transport does not add another generic audit outcome.
New control results and paused refusals contain safe state plus plain reasons
as structured JSON and identical text fallback. Unconfirmed saves require a
new status read and fault resolution, not a blind retry. No raw prompt, session,
message, path or parser input appears in these results.

## Dashboard Settings (v0.3.3)

A valid dashboard grant now authorizes persistent pool-size and iMessage-enable
changes. Anyone with that grant and network reachability, including a person
using an unlocked remembered phone profile, can change future agent capacity
and notification preference. Turning notifications off can hide future sign-in
delivery. Pool increases permit later dispatch within existing templates; they
cannot supply new executables or permission bypasses. Settings remain saved
after browser expiry, sign-out or grant rotation. Sign out neither undoes
settings nor revokes another device's grant.

GET/POST `/settings` use exact Host/port and header-token guards; POST additionally
requires exact Origin and JSON content type. No query grant, cookie auth, CORS,
generic config editor, service control or credential handoff is added. Paths,
models/effort, templates and execution options remain read-only/local-only.
Recipients, raw arguments and continuation text never enter the DTO or audit.
All dynamic content uses text nodes; CSP is unchanged.

Requests are limited to 8 KiB and four allowed changes. Unknown/duplicate keys,
nulls, coerced/string/fractional sizes, trailing JSON and read-only keys are
refused. Configuration stays at most 64 KiB; the explicit response is capped
at 32 KiB (oversize means unavailable, not a truncated complete snapshot).
Authenticated server-wide token buckets allow GET 60/minute, burst 10 and
POST 10/minute, burst 3, with Retry-After. There is no forwarded-IP limiter map.
Body reads have a five-second deadline. Settings GET shares a cancellable
one-second wait across its settings, manager and control locks. POST and audit
lock acquisition remain nonblocking; normal private-filesystem operations still
depend on the OS completing I/O.

Save uses CLI policy, cooperating control locks and one atomic mode-0600 write.
An opaque random revision maps to private config-byte identity; a stale client
gets 409. All fields validate before mutation; busy/surviving agents refuse
shrink. Legacy conversion remains local. Locks protect cooperating CLI writers,
not arbitrary owner-file editors that ignore the lock.

A private durable intent records byte identities and safe scalar fields before
mutation; an audit intent must also sync successfully. Each changed field gets
a durable outcome. Intent reconciliation at local dashboard construction or
the next Settings/CLI scalar write records committed, not_committed or uncertain
by current byte identity. HTTP GET has no write side effects. Config and audit
are separate files, so they are not one transaction.
After rename/directory-sync uncertainty the response says save_unconfirmed;
after an outcome failure it says committed_audit_failed. Both include a current
opaque revision when readable and require re-reading before retry. No rollback
is claimed or attempted. A crash during outcome append can produce repeated
reconciled outcomes; exactly-once audit delivery is not claimed.

## Remembered phone sign-in (v0.3.2)

The public, data-free shell removes the first-load credential query from the
address bar before storage or API work. It remembers the grant in localStorage
for this exact origin and browser profile, with an absolute seven-day expiry.
Reloads, polling and bookmarked opens do not renew that expiry. No credential
is put in subsequent URLs, asset requests, logs or cookies. API requests still
require `X-Nuntius-Token`; POST `/action` still requires the exact allowed
Origin. Exact Host/port checks, loopback binding, no CORS, same-origin CSP,
HTTPS through tailnet-only Serve and Funnel refusal remain unchanged.

This changes the threat model from one tab's lifetime to a remembered browser
profile. Someone using the same unlocked profile can open the bookmark and
control agents until expiry or revocation. Same-origin script compromise or a
privileged browser extension could read the persisted grant, including after
the tab closes; HttpOnly protection is not provided. Use only your own trusted
device/profile, protect it with a device lock, and sign out on shared devices.
Browser storage may be cleared by the OS or user, so persistence is not a
guarantee against storage eviction, private browsing or disabled storage.

Sign out removes this origin's stored grant and reloads the public shell to
discard private data and drafts. Other open tabs react to storage changes;
in-flight responses cannot restore signed-out data. Expiry is checked before
every API request and before rendering responses, including after sleep.
Malformed, expired or implausibly far-future records are discarded. A server
403 clears remembered sign-in; network/5xx failures retain it for retry.
When storage is disabled, a fresh link works in memory for this load only,
with the same time limit and sign-out control.

The seven-day limit is browser retention policy, based on the device clock;
it is not a server-enforced grant expiry. Sign out is local, not revocation:
other devices and copies of the original link retain access. `nuntius web stop`
ends serving; the next start creates a new random grant, rejecting all prior
links and stored copies. Never use a restart as a proof against owner services.
Offline tests use a fake clock, fresh tab contexts and temporary manager
fixtures to check persistence, expiry, sign-out, guards and grant rotation.

Alternatives considered: an HttpOnly, Secure, SameSite=Strict cookie would
reduce script-readable persistence but replace the browser's header-token
contract and require a larger authentication/CSRF change. Helper or
`nuntius web link --notify` re-sign-in keeps tab-only storage but does not make
a bookmark survive tab discard. Origin-scoped localStorage is the smallest
change that solves that loss while preserving existing server guards.

## Judge-only recovery

The `nuntius_web_ensure` capability belongs only to the existing
owner-launched stdio MCP server in the judge's host. Set approval to Ask on
each call. MCP annotations are descriptive; they provide no caller identity
or approval enforcement. Never register the server in coding-agent lanes or
relax their sandbox, storage or IPC permissions.

"Judge-only" ensure is enforced by MCP registration and OS user authority,
not by caller authentication. Any stdio client that can launch a separate
server with access to the owner configuration, store and system transports
can call ensure. Same-user file modes do not isolate an unsandboxed lane.
Host Ask approval applies in the registered host; it is not a server-side
identity check. Actual lane access restrictions need separate owner validation.

The request is an empty object. No executable, argv, path, address, recipient,
PID, grant or Tailscale setting is caller-selectable. Store/config come from
the owner process. The default listener is loopback-only, and a validated prior
record preserves its loopback port. Private directories remain 0700 and state,
access and sockets remain 0600. The lifecycle lock is nonblocking; malformed,
linked or inaccessible state cannot be interpreted as absence. Kernel start
identity is checked before every signal. Unknown jobs and occupied ports are
refused; shutdown has no force escalation.

Preflight accepts Running Tailscale with its exact MagicDNS name in the HTTPS
certificate domains. Funnel on any port or foreign 443 configuration refuses
recovery. Fixed argv, closed stdin, bounded output and deadlines prevent
interactive login/certificate consent or arbitrary commands. No login, up,
down, Funnel, reset, settings repair or installation command is issued.
Mapping cleanup re-reads current Serve state and removes only a still-exact
owned mapping. External configuration writes cannot be made atomic with the
Nuntius lock; failures and uncertainty remain visible.

Readiness authenticates through a request header over literal loopback HTTP
with the exact tailnet Host. It never follows redirects, uses environment
proxies, adds a query grant or reads/logs the data response body. Existing
Host, HTTPS Origin, header-only API auth, POST-only actions and CSP guards
remain in force. No public or unauthenticated ensure route or helper socket
operation is added. Tailscale ACLs and the dashboard grant both remain needed.

Responses contain allowlisted state words and a sign-in-needed boolean, never
private file contents, command output, paths, recipients, grants, links or QR.
Audit records contain the tool, operation class and outcome, without supplied
arguments. Grant creation can send one private link to the owner's enabled
configured recipient through the fixed Messages script on stdin. No grant or
recipient is added to process argv/environment. Notification failure does not
stop healthy web; reused grants are not resent.

Recovery is on demand and bounded to two minutes. It changes no agent freeze,
dispatch, cancellation or resume controls, and adds no watchdog, login item or
KeepAlive policy. Mac sleep/logout, unavailable owner MCP, phone VPN/ACL and
authenticated phone access remain outside local readiness guarantees. Tests
use temporary stores, fake processes and compiled fake transports. A live
owner phone pilot requires separate approval and never disrupts owner services
as a proof technique.
# Queue metadata (v0.3.3)

The read-only Queue endpoint uses the existing Host/header-token/method/Origin
guards and same-origin CSP. Browser queries cannot select files. Reads are
bounded and descriptor-rooted with no-follow/nonblocking opens and regular-file
checks; errors return fixed codes, with no source path, raw JSON or parser text.
Only validated known fields enter the response, followed by existing redaction.
Unknown v1 fields are ignored, never echoed; unsupported versions fail.
Titles/reasons can still be sensitive. Coordinators must omit credentials,
prompts, execution records and private access links, even in unknown fields.
Redaction is not a complete secret detector. Brief links are optional validated
external HTTPS links, opened only by a user with noreferrer/noopener; the server
does not fetch them. Queue never approves, dispatches, writes or repairs a file.
Sign-out and expiry clear private rows and reject late in-flight responses.
# Dashboard lane controls (Unreleased)

Lane and held-message writes use the existing exact loopback Host and port or
opted-in tailnet HTTPS Host, current header grant and matching POST Origin.
These checks run before body parsing. Query arguments do not authenticate or
select anything. The version 1 JSON schema rejects extra or duplicate keys,
null, aliases, coerced numbers and non-boolean flags. Lane control uses the
saved control revision; held review uses full run identity and content revision
and digest, never caller-selected files, session IDs or replacement text.

Settings, lane controls, held-message changes and existing actions share one
authenticated mutation allowance of ten per minute, with a burst of three.
Rejected authentication does not spend it. New control requests have a 4 KiB
body limit, five-second server read deadline and 32 KiB JSON response limit.
Mutations use immediate manager TryLock and nonblocking shared control flock,
the existing Settings write precedent. They do not wait on a background worker
or acquire locks in reverse order. Settings reads keep their cancellable wait.

Errors contain fixed codes and plain reasons, never raw request data, private
paths or parser errors. A revision conflict requires a fresh read. A failed
durability step is uncertain even if the new state is visible; no transport
retry is safe by default. Pause/resume uses the manager's single durable audit
protocol with trusted web source. Reviewed-message operations use the existing
identity-bound durable receipts and final admission guard. Native or portable
queue availability grants no authority and is not needed for operational reads.

`GET /data` uses read-only observation and does not create files, repair control
intent, emit audit outcomes, signal, supervise or recover runs. Definitely ended
processes lose the running/draining projection; uncertain live identities remain
conservative. Holds and run outcomes remain separate. Dashboard access still
uses the same owner-selected store and OS authority; this adds no caller role,
new grant, service, registration or permission bypass.
