package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

// Ollama's compose DNS name. Any other provider is LLM_BASE_URL plus
// LLM_API_KEY, no code change.
const (
	defaultLLMBaseURL = "http://ollama:11434/v1"
	defaultLLMModel   = "llama3.2:3b"
)

// OpenAIResponder streams a reply from any provider speaking the
// OpenAI-compatible /chat/completions API.
type OpenAIResponder struct {
	BaseURL string       // no trailing slash
	Model   string       // e.g. llama3.2:3b
	APIKey  string       // empty for a local Ollama; sent as a Bearer token when set
	System  string       // grounding prompt; empty sends none
	Client  *http.Client // injected so tests can point at an httptest server
}

// newLLMClient sets no Client.Timeout — a long generation is normal — but
// bounds the header wait so a wedged provider cannot pin a goroutine forever.
// A cold VRAM load measured 32.8s on a GTX 1060.
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

// openAIChunk is one SSE frame's JSON payload. Text deltas, finish_reason and
// usage each arrive in separate frames. Usage is a pointer so a frame without
// it is distinguishable from one reporting zeros.
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

// Excludes the space after the colon: the SSE grammar makes it optional, so
// Stream trims at most one afterward.
const sseDataPrefix = "data:"

// The only end-of-stream signal — the final JSON object carries no done flag.
const sseDoneSentinel = "[DONE]"

// providerError counts a provider failure and returns the gRPC status. Every
// provider failure path goes through here so the counter and the status code
// cannot drift apart. Client cancellation must NOT: chat.go's classifyOutcome
// needs the bare context error.
func providerError(reason string, code codes.Code, format string, args ...any) error {
	chatProviderErrorsTotal.WithLabelValues(reason).Inc()
	return status.Errorf(code, format, args...)
}

// readErrorBody summarises a provider's error response for the status message.
// Bounded: this text reaches the browser, and an error page can be any size.
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

// Stream returns Usage on every exit path, including errors: a stream that
// dies late has still reported real token counts.
func (o *OpenAIResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	var usage Usage

	body, err := json.Marshal(openAIRequest{
		Model:         o.Model,
		Stream:        true,
		StreamOptions: openAIStreamOptions{IncludeUsage: true},
		Messages:      o.messages(req.GetMessages()),
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
		// Omitted when unset: a bare "Bearer " is a malformed credential.
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	start := time.Now()
	resp, err := o.Client.Do(httpReq)
	if err != nil {
		// Cancellation arrives wrapped in a transport error. Return it bare so
		// classifyOutcome sees a hangup, not a dead provider.
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
			// Ollama 404s on an unpulled model; a hosted provider 404s on a
			// model it does not serve. One message covers both.
			return usage, providerError("model_missing", codes.Unavailable,
				"model %q not available at %s: %s (for a local Ollama, run: ollama pull %s)",
				o.Model, o.BaseURL, detail, o.Model)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// Internal, not Unauthenticated: our configuration is at fault, not
			// the browser's credentials.
			return usage, providerError("auth_error", codes.Internal,
				"llm provider rejected our credentials (HTTP %d): %s; check LLM_API_KEY", resp.StatusCode, detail)
		case resp.StatusCode == http.StatusTooManyRequests:
			return usage, providerError("rate_limited", codes.Unavailable,
				"llm provider rate limit hit (HTTP 429): %s", detail)
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// Internal, not Unavailable: a 4xx means our request is wrong and
			// can never succeed on retry. Unavailable would invite the
			// frontend's retry layer to loop against a permanent failure.
			return usage, providerError("http_error", codes.Internal,
				"llm provider rejected the request (HTTP %d): %s", resp.StatusCode, detail)
		default:
			return usage, providerError("http_error", codes.Unavailable,
				"llm provider returned HTTP %d: %s", resp.StatusCode, detail)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	// The default 64KB per line is not enough for a provider that batches deltas.
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
		// Optional per the SSE grammar: a gateway in front of the provider may
		// not send it.
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
			// finish_reason and usage frames carry an empty delta.
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
		// A cancelled read lands here, not at the loop's ctx check, because
		// Scan() returns false without completing a line. Bare, because
		// status.Errorf's %v would break the chain classifyOutcome matches on.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return usage, ctxErr
		}
		return usage, providerError("decode_error", codes.Internal, "reading completions stream: %v", err)
	}

	// No [DONE]: the provider died mid-generation — unless the client hung up
	// on the last frame, which reaches here with a clean EOF.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return usage, ctxErr
	}
	return usage, providerError("decode_error", codes.Internal,
		"completions stream ended without a [DONE] sentinel")
}

// messages prepends the system prompt to the mapped history. It has to lead
// the array: a provider that sees it after the history reads it as
// conversation, not instruction.
func (o *OpenAIResponder) messages(msgs []*chatpb.Message) []openAIMessage {
	if o.System == "" {
		return toOpenAIMessages(msgs)
	}
	return append([]openAIMessage{{Role: "system", Content: o.System}}, toOpenAIMessages(msgs)...)
}

// toOpenAIMessages maps proto roles to the wire format's role strings.
// ROLE_UNSPECIFIED has no mapping; validateHistory rejects it first.
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

// withSystem returns a copy bound to one turn's grounding prompt. System is a
// construction-time field but retrieval produces a new prompt every turn; the
// *http.Client is shared, so this is a five-field copy.
func (o *OpenAIResponder) withSystem(system string) Responder {
	clone := *o
	clone.System = system
	return &clone
}

// condensePrompt folds a follow-up into a standalone question. The model is
// told to echo the question back verbatim when it already stands alone, so a
// first-person question does not drift into a third-person paraphrase.
const condensePrompt = `Rewrite the user's final message as a standalone question that can be understood with no conversation history. Resolve pronouns and references using the conversation. Output only the question, with no preamble. If the final message already stands alone, output it unchanged.`

// condenseMaxTokens bounds the rewrite. A standalone question is one sentence;
// anything longer is the model answering instead of rewriting.
const condenseMaxTokens = 96

// condense runs a non-streaming completion against the same model. It lands
// ahead of the first token, so its latency is measured separately.
func (o *OpenAIResponder) condense(ctx context.Context, msgs []*chatpb.Message) (string, error) {
	payload := append([]openAIMessage{{Role: "system", Content: condensePrompt}}, toOpenAIMessages(msgs)...)
	body, err := json.Marshal(struct {
		Model     string          `json:"model"`
		Stream    bool            `json:"stream"`
		MaxTokens int             `json:"max_tokens"`
		Messages  []openAIMessage `json:"messages"`
	}{Model: o.Model, Stream: false, MaxTokens: condenseMaxTokens, Messages: payload})
	if err != nil {
		return "", fmt.Errorf("encoding condense request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building condense request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := o.Client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("condense request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("condense returned HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding condense response: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("condense returned no content")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
