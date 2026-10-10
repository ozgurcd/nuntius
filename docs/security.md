# Dashboard recovery security notes

The unreleased `nuntius_web_ensure` capability belongs only to the existing
owner-launched stdio MCP server in the judge's host. Set approval to Ask on
each call. MCP annotations are descriptive; they provide no caller identity
or approval enforcement. Never register the server in coding-agent lanes or
relax their sandbox, storage or IPC permissions.

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
