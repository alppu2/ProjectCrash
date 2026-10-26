# Web security: abuse and cost controls

*2026-10-11 — roadmap step 6*

## Goal

The chat endpoint is unauthenticated and, once live, backed by a third-party
model API billed per token. The provider's spend cap is global: one visitor
who exhausts it takes the assistant down for everyone until it resets. This
step makes sure no single visitor can spend more than a fixed daily share,
that floods are rejected cheaply at the edge, and that spend is visible and
alerted on before the cap is reached.

Success:
- A visitor's tokens per UTC day are capped, and they can see their balance.
- Site-wide tokens per UTC day are capped below the provider's spend cap.
- Floods, oversized bodies and cross-site embedding are stopped at Envoy.
- Concurrent streams are capped, rejected immediately, never queued.
- Spend pace, cap hits and abuse spikes raise alerts through Alertmanager.

Out of scope: the deployment topology in front of Envoy (Cloudflare, Caddy or
none) and the alert receiver, both decided in step 7; a frontend spend
display, which this step makes frontend-only work by shipping the proto
fields; a persistent or shared ledger.

## Layers

Each layer stops what it can measure, as cheaply as possible.

| Layer | Where | Stops |
|---|---|---|
| Edge hygiene | Envoy | Bodies over 64 KB, cross-site embedding, the dead catch-all route, an unpatched proxy |
| Request rate | Envoy `local_ratelimit` | Request floods per client IP, including `Warmup` |
| Concurrency | chat-service | Too many simultaneous streams, globally and per visitor |
| Token budget | chat-service | Per-visitor and site-wide daily spend |
| Per-turn ceiling | `OpenAIResponder` | One turn's output length, which bounds ledger overshoot |
| Alerts | Prometheus rules → Alertmanager | Spend pace, cap hits, refusal spikes, a degraded guard |

`Warmup` needs no layer of its own: `warmCache` shares one in-flight call and
reuses a success for 20 minutes, so a crawler storm costs at most one provider
call per 20 minutes. The request rate limit covers the flood.

## Envoy

- **Image** pinned to the current supported release at an exact version.
  `v1.25-latest` is out of support and floats across patch builds.
- **Client identity.** `use_remote_address: true` and `xff_num_trusted_hops`
  (0 now; step 7 sets it for whatever sits in front). Envoy then writes the
  resolved client address to `x-envoy-external-address`, overwriting anything
  the client sent. chat-service reads only that header.
- **Body limit.** `envoy.filters.http.buffer` with `max_request_bytes` 64 KB.
  Buffering is safe here because every request is unary; only `Chat`'s
  response streams. 64 KB sits above the handler's 32 KB history limit, so
  the handler's clearer error wins below it.
- **CORS** allow-origin comes from a single configured origin instead of `.*`.
  Envoy does not expand env vars in its config, so the allowed origin is a
  literal in `envoy.yaml`, set to the dev origin now and to the domain in
  step 7.
- **Routes.** The `/` catch-all to `order_service_cluster` is removed while
  the order path is parked; restoring it restores the route. Unknown paths
  get 404.
- **Rate limit.** `envoy.filters.http.local_ratelimit` with a descriptor on
  the client address, token bucket per IP. Starting values: 30 requests per
  minute, burst 10. The numbers live in `envoy.yaml`, a mounted file that
  becomes a ConfigMap later.

## chat-service

### Ledger: `internal/budget`

A new package with no gRPC imports. It holds, for the current UTC day, the
tokens each visitor has used, the site-wide total, and each visitor's open
streams.

- **Visitor key.** The client address from `x-envoy-external-address`. IPv4
  as-is; IPv6 truncated to its /64, since one host controls a whole /64. A
  missing or unparsable header is refused, never pooled into a shared
  "unknown" visitor.
- **`Acquire(key) (Balance, release func(), error)`** runs before the guard.
  It refuses if the site-wide or per-visitor open streams are at their cap, or
  if the visitor or the site has no tokens left today. `release` is deferred
  by the handler and is idempotent, so every exit path, including a cancelled
  stream, frees the slot.
- **`Debit(key, tokens)`** runs after the turn with the usage the provider
  reported. Charging after means a visitor near the limit can start one more
  turn and overshoot; `MAX_OUTPUT_TOKENS` bounds that to one turn.
- **What is charged.** Every model call the turn made: the condense rewrite
  and the main reply, input and output. `Usage` gains the condense call's
  tokens; today they are not reported. A guard-refused turn reaches no model
  and is charged nothing. Embeddings are not charged; they run locally.
- **Daily reset** is lazy: each call compares today's UTC date with the
  ledger's, and a new day replaces the ledger. The clock is injectable.
- **Memory bound.** Entries vanish at midnight and cost about 100 bytes each.
  Beyond `BUDGET_MAX_VISITORS` distinct visitors in a day, new visitors are
  refused: fail closed, like the guard.
- **Restart resets every budget**, site-wide included. Accepted: the
  provider's spend cap is the hard backstop, and the spend alerts read
  Prometheus, which survives restarts. A second chat-service replica would
  need a shared ledger; that belongs to the Kubernetes step.

### Configuration

All in `services/chat-service/.env.example`, mapping one-to-one onto a
ConfigMap later. Zero, negative, missing or unparsable is a startup error,
never "unlimited".

```
BUDGET_TOKENS_PER_VISITOR=20000   # per UTC day
BUDGET_TOKENS_GLOBAL=500000       # per UTC day; set below the provider spend cap
BUDGET_MAX_VISITORS=100000
MAX_OUTPUT_TOKENS=800             # per turn
MAX_STREAMS_GLOBAL=16
MAX_STREAMS_PER_VISITOR=2
```

The budget applies in echo mode too, with echo reporting zero usage, so the
plumbing is exercised without a model.

### Refusals

All three are `codes.ResourceExhausted` with an `errdetails.ErrorInfo` whose
`Reason` the frontend switches on, never the message text:

| Reason | Cause | Metadata |
|---|---|---|
| `TOO_MANY_STREAMS` | Concurrency cap, visitor or site-wide | — |
| `BUDGET_EXHAUSTED` | Visitor's daily tokens spent | `resets_at` (RFC 3339) |
| `GLOBAL_BUDGET_EXHAUSTED` | Site's daily tokens spent | `resets_at` |

Envoy's rate limit answers HTTP 429 before chat-service sees the request. How
that surfaces through gRPC-Web in connect-es is verified during
implementation, and the frontend detects it on that basis.

### Proto

```proto
message Balance {
  int32 tokens_used = 1;
  int32 tokens_remaining = 2;
  google.protobuf.Timestamp resets_at = 3;
}
message Done { ...; Balance balance = 5; }      // after this turn's debit
message WarmupResponse { Balance balance = 1; }  // on page load
```

Regenerated on both sides and committed, per the codegen rules.

### Metrics

- `chat_streams_total{status}` gains `rejected_concurrency` and
  `rejected_budget`, recorded through the existing single deferred
  accounting, not a second increment.
- `chat_budget_refusals_total{reason}` with the three reasons above.
- `chat_budget_global_used_tokens` gauge, reset at the UTC day boundary.
- `chat_budget_visitor_tokens` histogram, observed at each debit with the
  visitor's running total: a spend distribution for tuning the per-visitor
  cap. No visitor identity in any label.

## Frontend

- `useChatStream` keeps the latest `Balance` from `Done` and from `Warmup` in
  state; nothing renders it yet.
- Each refusal reason and the rate limit get fixed copy, shown in the chat in
  place of a reply. Draft wording, to be reviewed like the guard refusal:
  - rate limit: "Slow down a moment and try again."
  - `TOO_MANY_STREAMS`: "Finish your other conversation first."
  - `BUDGET_EXHAUSTED`: "You've reached today's limit. It resets at {local time}."
  - `GLOBAL_BUDGET_EXHAUSTED`: "The assistant is resting for today. Back at {local time}."

## Alerting

Prometheus evaluates rules from `infra/prometheus/rules/`; a new
`alertmanager` compose service receives them, configured in
`infra/alertmanager/alertmanager.yml`. Its receiver is a placeholder until
step 7 picks one; until then alerts are visible in the Alertmanager UI
(`:9093`) and in Grafana through an Alertmanager datasource.

| Alert | Expression (sketch) | For |
|---|---|---|
| `TokenSpendPace` | `sum(increase(chat_tokens_total[24h]))` > 70% of the global budget | 5m |
| `GlobalBudgetExhausted` | any `GLOBAL_BUDGET_EXHAUSTED` refusal in 10m | 0m |
| `BudgetRefusalSpike` | refusals of any reason above a threshold per 10m | 10m |
| `GuardDegraded` | `chat_guard_checks_total{verdict="error"}` share above 10% | 10m |

Spend alerts read `chat_tokens_total` from Prometheus rather than the
ledger's gauge, so a chat-service restart does not hide the day's spend. The
window is a rolling 24h, not the UTC day the ledger uses: PromQL has no
"since midnight", and a rolling window can only fire earlier, never later.
The global budget value reaches the rules as a constant in the rules file, kept
in step with `BUDGET_TOKENS_GLOBAL`; a NOTE marks both places, as with the
model names.

## Testing

- `internal/budget`: reserve and debit; refusal at zero for visitor and site;
  overshoot bounded to one turn; midnight reset under a fake clock; the
  visitor cap refusing new keys; concurrency acquire and release, including
  release on cancellation and double release; IPv6 /64 bucketing.
- Handler: a guard-refused turn is charged nothing; condense tokens are
  charged; each refusal carries the right code, reason and `resets_at`; a
  missing client-address header is refused; `Done.balance` reflects the
  debit; stream status is recorded exactly once.
- `OpenAIResponder`: the streaming request carries `max_tokens`.
- Config: zero, negative and missing caps fail startup.
- Frontend (vitest): each reason's copy; balance from `Done` and `Warmup`
  reaches state.
- Alert rules: `promtool test rules` cases for each alert, run in the
  Prometheus image.
- Envoy, a scripted check against the compose stack: 70 KB body → 413; burst
  → 429; a spoofed `X-Forwarded-For` does not change the bucket; a foreign
  `Origin` gets no CORS allow; `/orders.OrderService/` → 404.
