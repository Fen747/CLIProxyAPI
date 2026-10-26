# Fork operations and deployment readiness

## Scope and baseline

This fork adds a place to maintain subscription-aware routing without changing
client endpoints or enrolling existing accounts again. No routing changes or
deployment have been performed as part of this assessment.

The `fleet` branch starts at upstream `v8.0.29`, commit
`5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3`, matching the inspected running
server. Keep upstream `main` as the upstream tracking line. Bring upstream
updates into `fleet` deliberately, after reviewing configuration and storage
compatibility; do not deploy upstream `main` automatically.

## Current evidence

The authenticated management API confirmed version 8.0.29, a loopback listener,
an external file-based OAuth directory, `fill-first`, session affinity enabled,
and one active Claude credential plus one active Codex credential.

Existing operator records describe a Linux amd64 no-plugin executable managed
by a systemd user service, separate configuration/state directories, and a
private HTTPS endpoint forwarded to its loopback port. The operator's SSH
transport timed out during this assessment. The current service unit, executable
checksum, filesystem permissions and active connections have therefore **not**
been independently rechecked on the host. Do that before deployment. Keep
hostnames, keys, account identifiers and private host evidence outside this
public fork.

## Replacement assessment

A build from the same upstream commit can use the existing configuration,
OAuth directory, client keys, management environment, static management panel,
working directory and listener port. Clients and the HTTPS forwarder need no
configuration change if those values stay the same. This compatibility is
established from the configuration and code, not from a completed host trial.

Two limitations prevent a promise of transparent replacement:

* `internal/api/server.go`, `Server.Stop`, closes the HTTP server immediately.
  Restarting it can terminate active streams and WebSockets. A systemd stop
  timeout does not add graceful draining to that implementation.
* `sdk/cliproxy/auth/session_cache.go` keeps affinity bindings in memory. A new
  process loses them. Upstream prompt caches are not deleted by a proxy restart,
  but choosing a different account afterwards can prevent their reuse.

The first transition from the current process cannot import its bindings unless
an existing supported export mechanism is found. The current assessment found
none. Future releases should persist and reload affinity state, including
parent/fork aliases and expiration, before claiming restart continuity.

## Recommended release layout

Use immutable release directories and one stable executable path:

```text
proxy/
  releases/<release-id>/cli-proxy-api
  current -> releases/<release-id>
```

Make a one-time, narrowly scoped systemd `ExecStart` override point at
`proxy/current/cli-proxy-api`. Preserve all original arguments, environment
files, working directory, restart policy and permissions. Keep configuration,
credentials, management assets and mutable state outside release directories.
Never overwrite an executable that a running process is using.

Publish a pinned Linux amd64 no-plugin artifact, build version/commit/date and
SHA256 checksum. Use distinct fork release identifiers. A dedicated fork
release workflow should build only required targets and must not rely on the
upstream DockerHub destination or credentials. The inherited release workflow
is not yet a verified fork delivery pipeline.

## Deployment transaction

The eventual updater should default to a read-only plan and require an explicit
apply action. It should perform this sequence:

1. Verify host identity, architecture, service ownership, current binary and
   configuration compatibility. Acquire a deployment lock and remember the
   previous release target.
2. Download and verify the immutable artifact before touching the running
   service. Record its expected version and commit. Keep a private backup of
   the service definition and configuration with restrictive permissions.
3. Stage a health check on a separate loopback port with disposable state.
   Do not start two OAuth-refresh writers against the production credential
   directory or refresh copied production tokens during staging.
4. Stop admission of new inference work and drain active work using a verified
   mechanism. Until an admission/drain mechanism exists, use an observed idle
   maintenance window; a single idle connection snapshot is not a race-free
   guarantee. Abort rather than force interruption when work remains active.
5. Switch the symlink atomically, restart only the proxy service, and verify the
   expected binary, listener and authenticated management health.
6. Verify the unchanged external endpoint, rejection of unauthenticated calls,
   account presence, model listing, and a controlled inference request for each
   provider. Record success before deleting any previous release.
7. On failure, restore the previous symlink and restart the previous binary.
   Preserve the latest OAuth files: binary rollback must not automatically
   restore older refresh tokens. Restore configuration only when required and
   proven compatible with its current contents.

The service interruption should be limited to restart and health verification
after artifact staging, but its duration is unmeasured. Do not claim zero
downtime. A future persistent frontend with admission control and a draining
backend can support a more continuous handover; it also needs a single OAuth
refresh owner and durable affinity state.

## Subscription routing work to follow

Keep existing affinity, parent/fork lookup, priority, cooldown and failover
behavior. Apply subscription-aware ranking only to an initial selection with
no reusable affinity. Explicitly distinguish this from failure recovery.

Maintain a fresh per-account quota snapshot, combining passive response headers
with bounded reads of the OAuth usage endpoint. Prefer useful available capacity
whose five-hour reset is approaching, with near-exhaustion margins, applicable
weekly limits, capacity weights and deterministic fallback when data is stale
or unavailable. Concurrent initial selections must not all overcommit one
account solely because they saw the same snapshot.

Make hourly checks and window priming separately configurable. A zero utilization
percentage alone is not proof of an inactive window. Confirm reset state, pin
any priming request to the intended account, prohibit cross-account failover,
and verify the resulting window. Bound attempts, deduplicate per account and
persist enough state to prevent repeated priming after restarts. Schedule near
known resets as well as the hourly recovery scan. Test the actual provider
semantics before enabling priming; it does not increase weekly allowances.

## Acceptance before a live switch

Verify existing settings and accounts survive binary replacement and rollback;
active streams are drained or deployment is refused; repeated updates are
idempotent; invalid artifacts leave the live service unchanged; failed startup
recovers the old binary; secrets never appear in logs; and a quota-data outage
falls back without breaking sessions. Exercise affinity persistence across a
restart once that feature exists. Validate host-specific instructions privately
and keep observed results distinct from this proposed procedure.
