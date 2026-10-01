# Chat Warmup and Welcome — Design

**Date:** 2026-10-01
**Status:** Draft, awaiting review

The page opens on an empty chat, so a visitor has no cue for what it is
for, and the first question pays the model's cold load (~33s for
`llama3.2:3b` on the local GPU). This design warms the assistant when the page
loads, shows a loading state while that happens, and then shows a fixed
welcome message that explains what the chat answers.

## Decisions

| Question | Choice | Why |
|---|---|---|
| Who writes the welcome | Fixed copy, authored by hand | The welcome is the first thing an evaluator reads. A 3B model generating it per visit could state things about a real person that are not true, and would cost a full generation per page load. |
| Where the copy lives | Frontend, `welcome.ts` | It is page copy, not model output. Editing it should not rebuild a Go image. |
| How warmup is triggered | New unary RPC `ChatService.Warmup`, called on mount | Keeps warmup (infrastructure) apart from the welcome (content). A unary status is a cleaner failure signal than an error mid-stream. |
| What warmup does in llm mode | One `max_tokens: 1` completion plus one embedding call | Loads both models on Ollama. On a hosted provider it verifies URL, key, model name and quota before the visitor types. Same request shape `condense` already sends, so any provider that serves chat serves warmup. |
| Hosted providers | Same code path, no `LLM_WARMUP` switch | The freshness cache caps provider calls at roughly one per window per replica, so hosted cost is ~10 tokens every 20 minutes. Add a switch only if a provider ever bills per request. |
| Repeat and concurrent calls | Single-flight plus a 20-minute freshness window; failures not cached | The endpoint is unauthenticated. Page loads must not translate into provider calls, and a recovered provider must be picked up by the next page load without a restart. |
| Echo mode | `Warmup` succeeds immediately | The GPU-less default stack shows the same warming → welcome flow. |
| Input while warming | Input disabled, placeholder explains why | A message sent before warmup would pay the cold load anyway, and would race the welcome into the list. |

The window is 20 minutes, below compose's `OLLAMA_KEEP_ALIVE: 30m`, so the
cache never reports warm for a model Ollama has already unloaded. The window
is measured from the last successful warmup only, which is conservative: real
chat traffic also keeps the model resident.

## Architecture

```
browser ──grpc-web──> Envoy :8080 ──grpc──> chat-service ──HTTP──> Ollama / hosted
  Warmup() on mount     route prefix          Warmup handler
                        /chat.v1.ChatService/      │
                        already matches            └── responder.Warmer (optional interface)
                                                         └── warmCache: single-flight + freshness
```

`envoy.yaml` is unchanged: its `/chat.v1.ChatService/` prefix route already
covers the new method.

### Proto

```proto
service ChatService {
  rpc Chat(ChatRequest) returns (stream ChatChunk);
  rpc Warmup(WarmupRequest) returns (WarmupResponse);
}

message WarmupRequest {}
message WarmupResponse {}
```

Empty messages leave room for fields later (e.g. a model id) without a new
RPC. Regenerate Go (`chat-service/chat/`) and TypeScript (`frontend/src/gen/`)
and commit both.

### chat-service

**`internal/responder`**

- `Warmer` interface: `Warmup(ctx context.Context) error`. Optional: a
  Responder that does not implement it is treated as always warm. `EchoResponder`
  does not implement it.
- `OpenAIResponder.warm(ctx)`: non-streaming completion, one user message,
  `max_tokens: 1`, sharing the request helper with `condense`.
- `RetrievingResponder.Warmup(ctx)`: embeds one short fixed string, then calls
  `Inner.warm`. Both calls run regardless of `EMBEDDER`: on a hosted embedder
  the call loads nothing but still proves retrieval can embed the question.
  Qdrant is not probed; startup already verified the collection.
- `warmCache` wraps a `Warmer`. A mutex guards `warmedAt` and an in-flight
  channel; ~30 lines, no `golang.org/x/sync` dependency.
  - Fresh (`now - warmedAt < 20m`): return nil without calling through.
  - In flight: wait on the shared result or the caller's `ctx`, whichever is
    first.
  - Otherwise start the work under `context.WithoutCancel(ctx)` plus a 120s
    timeout. A visitor closing the tab must not abort the load every other
    visitor is waiting on. 120s covers the 33s cold load with headroom.
  - On error, `warmedAt` is left unchanged, so the next call retries.
- `New` wraps the llm responder in `warmCache` and returns it as both
  `Responder` and `Warmer`.

**`chat.go`**

- `chatServer` gains `warmer responder.Warmer`, nil in echo mode.
- `Warmup` handler: nil warmer → return OK. Otherwise call it. An error becomes
  `codes.Unavailable` with a generic message; the provider detail goes to the
  log, not to the browser. Cancellation is classified with
  `responder.ClassifyOutcome`, as `Chat` does.
- One deferred func records `chat_warmups_total{outcome}` (`ok`, `error`,
  `cancelled`) and `chat_warmup_duration_seconds`, matching the
  single-site rule `Chat` follows.

**`metrics.go`** (root, alongside the stream counters): the two collectors
above. Duration buckets reuse the stream histogram's, since a cold load lands
around 30s.

### Frontend

**`welcome.ts`** exports `WELCOME_MESSAGE`. Draft copy, final wording is the
author's call:

> Hi, welcome to Aleksi Valta's portfolio. I'm an assistant that answers
> questions about his background, experience and this project, which runs on
> the stack you're talking to right now. Try asking what he has worked on, or
> how this chat is built.

**`useChatStream.ts`**

- Adds `status: 'warming' | 'ready' | 'unavailable'` and `retry()`.
- On mount: `chatClient.warmup({}, { signal, timeoutMs: 120_000 })`. Success
  sets `ready` and appends the welcome as an `ROLE_ASSISTANT` message, revealed
  word by word on a timer to match streamed replies. Failure that is not an
  abort sets `unavailable`. Unmount aborts. Under React StrictMode the double
  mount sends two calls, and the server's single-flight absorbs them.
- `send()` refuses unless `status === 'ready'`.
- The welcome never reaches the server. `trimHistory` already snaps the
  window forward to the first user turn, and `validateHistory` would accept
  it anyway since only the last message must be a user turn. A test pins this.

**`ChatPanel.tsx`**

- `warming`: a placeholder line "Assistant is warming up…" in the message list;
  input disabled with the same text as its placeholder.
- `unavailable`: "The assistant is offline right now." plus a Retry button.
- Plain markup and class names only. Styling and the progress indicator come
  later with a component library.

## Error handling

| Situation | What the visitor sees |
|---|---|
| Echo mode | Welcome appears almost immediately |
| Ollama cold | Warming state for ~30s, then the welcome |
| Ollama warm, or hosted provider healthy | Welcome within about a second |
| Bad API key, wrong model, 429, provider down | `unavailable` with Retry; detail in chat-service logs |
| chat-service down (e.g. llm startup crash loop) | Envoy returns 503, so `unavailable` with Retry |
| Warmup succeeds, provider fails later | Unchanged: the turn errors, rolls back, the typed text is restored |

## Testing

Go (`internal/responder`, `httptest` for providers):

- `warmCache`: concurrent callers trigger one call through; a fresh result
  skips the call; a stale one repeats it; an error is not cached; a cancelled
  caller does not cancel the shared work.
- `RetrievingResponder.Warmup` hits `/embeddings` and `/chat/completions`
  with `max_tokens: 1`; either failing fails warmup.
- Handler: nil warmer returns OK; an error maps to `codes.Unavailable` with no
  provider detail in the message.

Frontend (vitest, mocked `chatClient` as in `useChatStream.test.ts`):

- `warming` → `ready` appends the welcome; `warming` → `unavailable` on
  rejection; `retry()` returns to `warming`.
- `send()` is refused while warming.
- The request after the welcome does not include the welcome message.

## Out of scope

- Styled loading and progress UI (component library, later).
- chat-service's exit-at-startup behaviour in llm mode. This design surfaces it
  as `unavailable` instead of an empty chat, but does not change it.
- Grafana panels for the new metrics.
