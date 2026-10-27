# Durable local session affinity

The fork checkpoints explicit session aliases and Merkle prefix affinity when
running without Home. No new client settings or credential enrollment are needed.
The first upgrade from upstream cannot recover bindings that only existed in
the old process's memory.

## State and lifecycle

After credentials load and before serving, the service restores
`AUTH_DIR/.session-affinity.state`. It checkpoints every 30 seconds and once
before stopping the selector during shutdown. The non-JSON suffix keeps this
runtime file out of the credential loader and watcher. Configuration, OAuth
credentials and release directories are not rewritten by these checkpoints.

Versioned JSON contains opaque account/session identifiers, aliases, absolute
expirations, prefix/tail fingerprints and lineage metadata. It contains no OAuth
material or original conversation turns. Identifiers and fingerprints are private
metadata: the file is atomically replaced from a 0600 temporary file, with a
64 MiB limit. Import rejects symlinks, public permissions, unsupported versions
and malformed files. Invalid state warns and starts cold rather than blocking
the proxy. Expired bindings and missing/disabled credentials are discarded;
temporary quota cooldowns do not discard otherwise valid associations.

Import keeps eviction order and capacity bounds, rebuilds prefix/tail indexes and
access generations, and never refreshes expiration. Routing/TTL/rebind changes
share caches with the replacement selector, so late results still update the
same bindings. Only the last owner clears the matcher and stops cleanup. A new
TTL applies on refresh, without extending existing bindings on config reload.
Disabling affinity causes the next checkpoint to store empty state.

## Operational limits

This does not add request draining. Updates still require a drained, stopped
service as described in [fork operations](FORK_OPERATIONS.md). Shutdown saves
the state available at that point; in-flight requests need the separate draining
work. Abrupt termination can lose changes since the last successful checkpoint,
normally up to 30 seconds with healthy storage. No live rollout or zero-downtime
claim is made here.

Keep one writer per state directory. Restart after changing the auth directory
or switching Home ownership; the process stops writing to the former directory
when it detects that change. Upstream binaries ignore this file and cannot
maintain affinity continuity during a rollback to that baseline.

## Verification

Run `go test -race ./sdk/cliproxy/...` and `go build ./cmd/server`.
New tests cover explicit aliases and prefix affinity, fork/compaction lineage,
expiration and eviction, removed/disabled credentials, corrupt/private-file
boundaries, late results across selector replacement, concurrent checkpoints,
and service shutdown/startup. Assertions check that request text and OAuth
metadata stay out of snapshots. Tests do not consume provider tokens.
