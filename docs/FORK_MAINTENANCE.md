# Admission control and maintenance

The fork can stop admitting new requests while existing responses finish. This
is an explicit maintenance operation; ordinary shutdown keeps upstream's
immediate-stop behavior. No live deployment has been performed.

## Contract

All three methods on `/v8/management/server/maintenance` require the existing
management authentication and remote-access policy:

| Method | Action |
| --- | --- |
| GET | Read `maintenance`, `pid`, and `supported`; never exposes the operation token. |
| PUT | Close admission. Body: `token` and the `instance` returned by GET. Repeat with the same token to observe progress. |
| DELETE | Reopen admission. Body: the same owner token and the current instance. |

Tokens contain 32–128 ASCII letters, digits, `_` or `-`. The release tool generates
one, saves it privately, and never prints it. Another operation cannot resume or
take over that drain. Instance checks prevent a delayed request from modifying
a newly started process without first reading its state.

The response contains `instance`, `draining`, `active`, `ready` and `durable`
inside `maintenance`. Admission and the active count share one mutex. A ready
drain stays closed until its owner resumes. Rejected HTTP requests receive 503,
`proxy_draining` and `Retry-After: 1`.

The server stores its owner in `AUTH_DIR/.maintenance.state` (private, at most
128 bytes, no OAuth material). A restart retains closed admission, preventing a
service restart during deployment from opening an unobserved admission window.
The marker is removed when the owner resumes; malformed, public or symlink
markers fail startup. Keep one process per auth directory. Changing that
directory requires a restart. Upstream binaries do not understand this marker.

## What is counted

* HTTP requests, including full SSE delivery, count until their handler and
  middleware return. Arbitrary management API calls, plugin paths, and custom
  HTTP routes go through the same admission gate.
* Responses WebSockets release the HTTP handshake ticket and acquire a ticket
  for each turn. Idle sockets do not block maintenance. Both `/v1/responses`
  and `/backend-api/codex/responses` use this behavior. Maintenance rejects new
  create/append/steering input; interrupts remain available to finish existing
  work. A queued turn is checked again before execution.
* Continuous Responses steering remains counted until its upstream stream
  ends. Other opaque WebSockets count for the handler lifetime. Codex Live
  calls also count while retained after their HTTP bootstrap. These conservative
  counts can cause a bounded maintenance attempt to abort rather than cutting
  an active connection.
  Sideband connections and hangup for an already-stored call remain admitted
  and counted, with their normal authentication and ownership checks, so clients
  can finish those calls during maintenance.
* The websocket relay's provider connections remain open while admitted HTTP
  work uses them; provider registration alone is not an inference request.

Health, the management HTML, authenticated GET `/v8/management/config`, and
maintenance controls stay available. Other HTTP work is rejected during the
drain, including configuration edits and arbitrary management API calls.

Home-dispatched background work and SDK calls made outside this HTTP server are
not covered. Automatic maintenance is unsupported in Home mode or without a
durable auth directory. Do not change configuration, replace credentials or run
another deployment controller during an update. Local admission cannot stop
external provider-side jobs that intentionally outlive an HTTP request.

## Update and recovery

After the one-time adoption and service override, stage the verified artifact,
then use the same activation command with `--drain`:

```sh
python3 scripts/fork_release.py activate --release RELEASE_ID \
  --service ACTUAL_PROXY.service --management-key-file PRIVATE_MANAGEMENT_KEY \
  --drain --drain-wait 300 --apply
```

Without `--apply`, this verifies the managed service and maintenance endpoint
without closing admission. With `--apply`, the release tool verifies the old
binary's health, matches the API PID to systemd, closes admission, waits at most
the configured period, and stops the service only after a ready result. It then
uses the existing atomic release switch and authenticated health checks.
`rollback --drain` uses the same sequence.

If drainage times out or the operation is interrupted while waiting, it reopens
admission without stopping the service. If resuming fails, a private
`ROOT/.maintenance-operation.json` retains the owner token. Recover with:

```sh
python3 scripts/fork_release.py resume \
  --service ACTUAL_PROXY.service --management-key-file PRIVATE_MANAGEMENT_KEY --apply
```

Resume reads the live instance, then uses the saved token; a different owner's
maintenance remains protected. It can also recover the same durable drain after
a process restart. Never publish or manually delete operation files while an
update runs.

An unhealthy candidate is also checked/drained before automatic rollback. If its API
cannot prove idle, the tool leaves its release selected and fails for operator
recovery instead of forcibly stopping it. The server marker remains closed
across the next start; the release tool resumes its saved owner only after
checking health, including after rollback. Both releases must support durable
maintenance; an older upstream binary ignores the marker and cannot participate
in this automatic handover. The target manifest must declare
`maintenance_protocol: 1`; unsupported targets are refused before admission
changes, including an automatic rollback to a pre-maintenance release. Recover
any saved operation before using stopped-service activation. A normal health
check of a maintenance-aware release also verifies admission is open; only an
explicit `--drain` transaction allows closed admission during its health checks.
If admission cannot be resumed after the candidate passed health checks, it
stays selected and the private recovery record remains; use `resume` without
rolling back a healthy binary.

The currently live upstream build has no maintenance endpoint. The first
transition still needs an externally enforced idle window and an already
stopped service; automatic drainage applies to subsequent fork builds. Downtime
includes restart and health verification and has not been measured.
