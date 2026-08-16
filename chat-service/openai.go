package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

const (
	// Ollama runs as a compose service, so this is a DNS name on
	// micro-network — the same shape as a hosted provider's URL. Swapping to
	// Groq, OpenRouter or a self-hosted vLLM is LLM_BASE_URL plus LLM_API_KEY,
	// with no code change.
	defaultLLMBaseURL = "http://ollama:11434/v1"
	defaultLLMModel   = "llama3.2:3b"
)

// OpenAIResponder streams a reply from any provider speaking the
// OpenAI-compatible /chat/completions API: Ollama's /v1 surface locally, a
// hosted open-model API in the cloud. BaseURL and Client are fields rather
// than constants so tests can point at an httptest server.
type OpenAIResponder struct {
	BaseURL string       // e.g. http://ollama:11434/v1, no trailing slash
	Model   string       // e.g. llama3.2:3b
	APIKey  string       // empty for a local Ollama; sent as a Bearer token when set
	Client  *http.Client // injected; see newLLMClient for the production one
}

// newLLMClient sets no Client.Timeout — a long generation is normal, not a
// fault — but bounds the wait for response headers so a wedged provider fails
// instead of pinning a goroutine forever. A cold VRAM load measured 32.8s on a
// GTX 1060, so 120s leaves room without being unbounded.
func newLLMClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 120 * time.Second
	return &http.Client{Transport: transport}
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIRequest struct {
	Model         string              `json:"model"`
	Stream        bool                `json:"stream"`
	StreamOptions openAIStreamOptions `json:"stream_options"`
	Messages      []openAIMessage     `json:"messages"`
}

// openAIChunk is one SSE frame's JSON payload. The three interesting kinds
// arrive separately: a text delta, a frame carrying finish_reason with an empty
// delta, and a trailing usage-only frame with no choices at all. Usage is a
// pointer so a frame without it is distinguishable from one reporting zeros.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int32 `json:"prompt_tokens"`
		CompletionTokens int32 `json:"completion_tokens"`
	} `json:"usage"`
}

// sseDataPrefix marks a payload line in a server-sent-event stream. Other
// fields (event:, id:, retry:) and comment lines (:) are ignored. The space
// after the colon is optional per the SSE grammar — a parser must strip one
// leading space if present, not require it — so the prefix excludes it and
// Stream trims at most one afterward.
const sseDataPrefix = "data:"

// sseDoneSentinel terminates an OpenAI-compatible stream. Unlike Ollama's
// native NDJSON there is no done flag on the final JSON object, so this
// literal is the only end-of-stream signal.
const sseDoneSentinel = "[DONE]"

// providerError records why a provider call failed and returns the gRPC status
// the handler passes through unchanged. Every provider failure path goes
// through here so the counter and the status code cannot drift apart.
//
// Client cancellation must NOT come through here: it is expected, not a fault,
// and chat.go's classifyOutcome needs the bare context error.
func providerError(reason string, code codes.Code, format string, args ...any) error {
	chatProviderErrorsTotal.WithLabelValues(reason).Inc()
	return status.Errorf(code, format, args...)
}

// readErrorBody summarises a provider's error response for the status message.
// Bounded, because an error page can be arbitrarily large and this text ends up
// in a gRPC status the browser receives. OpenAI-shaped bodies get their
// message field lifted out; anything else is flattened to one line.
func readErrorBody(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 2048))
	if err != nil || len(raw) == 0 {
		return "no error body"
	}
	var wrapper struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &wrapper) == nil && wrapper.Error.Message != "" {
		return wrapper.Error.Message
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\n", " "))
}

// Stream accumulates Usage as frames land and returns it on every exit path,
// including errors: finish_reason and the token counts arrive in separate
// frames, so a stream that dies late has still reported real numbers. See the
// Usage doc comment in responder.go.
func (o *OpenAIResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	var usage Usage

	body, err := json.Marshal(openAIRequest{
		Model:         o.Model,
		Stream:        true,
		StreamOptions: openAIStreamOptions{IncludeUsage: true},
		Messages:      toOpenAIMessages(req.GetMessages()),
	})
	if err != nil {
		return usage, providerError("config_error", codes.Internal, "encoding completions request: %v", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return usage, providerError("config_error", codes.Internal, "building completions request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		// Omitted entirely when unset: a local Ollama needs no credential, and
		// a bare "Bearer " would be a malformed one.
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	start := time.Now()
	resp, err := o.Client.Do(httpReq)
	if err != nil {
		// A cancelled request surfaces here as a transport error wrapping
		// ctx.Err(); return the bare context error so the handler classifies
		// it as cancelled rather than as a dead provider.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return usage, ctxErr
		}
		return usage, providerError("unreachable", codes.Unavailable,
			"llm provider unreachable at %s: %v", o.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail := readErrorBody(resp.Body)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			// A local Ollama 404s on an unpulled model, which is the most
			// likely misconfiguration here; a hosted provider 404s on a model
			// name it does not serve. One message covers both.
			return usage, providerError("model_missing", codes.Unavailable,
				"model %q not available at %s: %s (for a local Ollama, run: ollama pull %s)",
				o.Model, o.BaseURL, detail, o.Model)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// codes.Internal, not Unauthenticated: the browser's credentials
			// are not at fault, our configuration is.
			return usage, providerError("auth_error", codes.Internal,
				"llm provider rejected our credentials (HTTP %d): %s; check LLM_API_KEY", resp.StatusCode, detail)
		case resp.StatusCode == http.StatusTooManyRequests:
			// Both a transient rate limit and a hard quota exhaustion arrive as
			// 429; detail is what tells them apart.
			return usage, providerError("rate_limited", codes.Unavailable,
				"llm provider rate limit hit (HTTP 429): %s", detail)
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// codes.Internal, not Unavailable: a 4xx is our request being wrong
			// — a too-long context, an unsupported parameter — and can never
			// succeed on retry. Unavailable is the canonical retryable code,
			// and CLAUDE.md puts retries in frontend/src/api.ts, so handing one
			// out here would invite a retry loop against a permanent failure.
			return usage, providerError("http_error", codes.Internal,
				"llm provider rejected the request (HTTP %d): %s", resp.StatusCode, detail)
		default:
			return usage, providerError("http_error", codes.Unavailable,
				"llm provider returned HTTP %d: %s", resp.StatusCode, detail)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	// Default 64KB per line is generous for a delta but not for a provider
	// that batches, so raise the ceiling rather than fail on a long frame.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	firstDelta := true
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return usage, err
		}

		payload, ok := strings.CutPrefix(scanner.Text(), sseDataPrefix)
		if !ok {
			continue // blank separator line, comment, or a non-data SSE field
		}
		// The SSE grammar makes the space after the colon optional and requires a
		// parser to strip one if present. Ollama and OpenAI both send it; a proxy
		// or self-hosted gateway in front of them may not.
		payload = strings.TrimPrefix(payload, " ")
		if payload == sseDoneSentinel {
			return usage, nil
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return usage, providerError("decode_error", codes.Internal,
				"decoding completions frame: %v", err)
		}

		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				usage.StopReason = choice.FinishReason
			}
			// The finish_reason and usage frames carry an empty delta;
			// emitting it would send a text_delta the client must ignore.
			if choice.Delta.Content == "" {
				continue
			}
			if firstDelta {
				chatTimeToFirstTokenSeconds.Observe(time.Since(start).Seconds())
				firstDelta = false
			}
			if err := emit(choice.Delta.Content); err != nil {
				return usage, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// A cancelled read surfaces here rather than at the loop's ctx check,
		// because Scan() returns false without completing a line. Return the
		// bare context error: classifyOutcome must see a hangup as cancelled,
		// and status.Errorf's %v would destroy the chain it matches on.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return usage, ctxErr
		}
		return usage, providerError("decode_error", codes.Internal, "reading completions stream: %v", err)
	}

	// Ran out of frames without a [DONE]: the provider died mid-generation —
	// unless the client hung up on the last frame, which reaches here with a
	// clean EOF and must stay a cancellation rather than a provider fault.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return usage, ctxErr
	}
	return usage, providerError("decode_error", codes.Internal,
		"completions stream ended without a [DONE] sentinel")
}

// toOpenAIMessages maps proto roles to the wire format's role strings.
// ROLE_UNSPECIFIED has no mapping, which is why validateHistory rejects it
// before a request reaches here.
func toOpenAIMessages(msgs []*chatpb.Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.GetRole() == chatpb.Role_ROLE_ASSISTANT {
			role = "assistant"
		}
		out = append(out, openAIMessage{Role: role, Content: m.GetContent()})
	}
	return out
}
