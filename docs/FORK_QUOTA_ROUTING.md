# Initial Claude account selection by subscription quota

Opt in without changing existing bindings:

```yaml
routing:
  strategy: weighted-round-robin
  session-affinity: true
  quota-aware-initial: true
```

The default is `false`. Home routing is excluded. This setting hot reloads. New
bindings use the highest eligible credential priority as before. Explicit session,
parent/fork and LCP matches remain authoritative, including across routing reloads.
Unavailable bindings and execution retries retain the configured failover strategy.
No-session requests, API keys, custom upstreams and non-Claude credentials retain
the configured strategy. A mixed candidate set also falls back to that strategy.

## Observations

The service reads `GET https://api.anthropic.com/api/oauth/usage` at startup and
hourly per Claude OAuth credential, with additional reads 30 seconds before and
5 seconds after known resets (15-second scheduling granularity). Failed reads retry
after one minute. Requests honor per-account/global proxy settings, reject redirects,
and bound encoded and decoded responses to 1 MiB. There is at most one read per
credential and four reads globally. No inference, token refresh or credential-file
write is performed. **Reading usage does not start a five-hour window.** Optional
window priming is a separate feature.

Snapshots are held only in RAM, keyed by credential ID, registration epoch and
credential version. Replacement/removal discards old observations and cancels the
old read; enabling the setting starts cold. Changes to proxy settings also cancel
old reads. Shutdown, disable and Home transitions cancel outstanding reads; config
changes are reconciled within 15 seconds. In accordance with the upstream timeout
policy, there is no post-connect request deadline. A stalled read occupies one of
the four slots until completion or lifecycle/config cancellation, without blocking
inference or account selection.

Usage fields are `five_hour`, shared `seven_day`, and model-specific
`seven_day_opus` / `seven_day_sonnet`. OAuth utilization is a percentage; passive
response-header utilization is a fraction. Explicit null windows are inactive;
missing applicable fields are unknown. Newer complete 5h/shared-weekly header pairs
can augment a fresh OAuth snapshot for the same credential version. The ambiguous
`7d_oi` headers are not interpreted as an Opus cap. Model aliases use their resolved
upstream model for the applicable weekly cap.

## Ranking and fallback

Every candidate must have comparable observations no older than 75 minutes and no
elapsed applicable reset. Otherwise the configured strategy selects normally.
Unknown model families likewise fall back. Account priority and weighted-strategy
exclusions remain in force. No quota decision can move an existing session.

For comparable candidates, the policy computes:

```
remaining = minimum remaining fraction across 5h, shared weekly, model weekly
score = max(0, remaining - 0.02) * weight / reset_horizon_hours / (1 + pending)
```

The 5h reset horizon is clamped to 5 minutes–5 hours; an inactive window uses
5 hours. This favors useful allowance expiring soon while accounting for weekly
limits and retaining a 2% advisory reserve. All scores at zero fall back to the
configured strategy. Ties use credential ID for reproducibility. Credential weight
expresses relative plan capacity (default 1); set it appropriately for unequal
plans. The API exposes utilization rather than an absolute spendable token count,
so this is a scheduling heuristic, not a token-capacity guarantee.

`pending` counts atomic, account-wide reservations for initial selections, shared
across models and routing reloads. The first execution result releases its reservation;
an abandoned result expires after one minute. These reservations reduce bursts of
new sessions onto a single account while keeping the chosen sessions sticky.
Observations/reservations are advisory and do not override upstream cooldowns.
There is no active-session migration, durable quota snapshot, extra-usage activation,
or automatic window priming.

## Verification

Run the relevant suites with the repository Go toolchain:

```sh
go test -race ./internal/auth/claude ./internal/config ./internal/watcher/diff ./sdk/cliproxy/auth ./sdk/cliproxy
go build -o /tmp/cliproxy-quota ./cmd/server
```

Tests exercise quota/reset/weekly ranking, weights and priorities, inactive and
malformed windows, stale/replaced observations, passive signals, existing explicit/
parent/fork/LCP bindings and unavailable matches, retry fallback, simultaneous
selection, reservation expiry/reload, bounded polling, cancellation and stale results,
read-only OAuth requests, redirects and response-size limits. No live credentials or
inference are needed.
