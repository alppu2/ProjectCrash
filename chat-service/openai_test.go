package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

// Frames as captured from a real provider in Task 1 Step 4. Text deltas arrive
// at choices[0].delta.content; finish_reason arrives on a frame whose delta is
// empty; token counts arrive in a trailing usage-only frame with no choices.
const (
	frameHel    = `data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`
	frameLo     = `data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`
	frameFinish = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	frameUsage  = `data: {"choices":[],"usage":{"prompt_tokens":26,"completion_tokens":298}}`
	frameDone   = `data: [DONE]`
)

// sseServer serves the given frames as a server-sent-event stream, flushing
// each so a reading client sees them arrive separately rather than in one
// buffer. Frames are written verbatim, so a test can pass malformed input.
func sseServer(t *testing.T, status int, frames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		for _, f := range frames {
			io.WriteString(w, f+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestLLM(baseURL string) *OpenAIResponder {
	return &OpenAIResponder{BaseURL: baseURL, Model: "llama3.2:3b", Client: &http.Client{}}
}

func TestOpenAIResponderStreamsDeltasAndUsage(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameLo, frameFinish, frameUsage, frameDone)
	r := newTestLLM(srv.URL)

	var got []string
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	// Deltas travel verbatim: the provider already includes leading spaces, so
	// unlike EchoResponder there is no separator to reconstruct.
	if want := []string{"Hel", "lo"}; !slices.Equal(got, want) {
		t.Errorf("deltas = %q, want %q (empty-delta frames must be skipped)", got, want)
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
	if usage.InputTokens != 26 {
		t.Errorf("InputTokens = %d, want 26", usage.InputTokens)
	}
	if usage.OutputTokens != 298 {
		t.Errorf("OutputTokens = %d, want 298", usage.OutputTokens)
	}
}

// A provider that ignores stream_options sends no usage frame. The stream must
// still succeed with whatever it did report — Ollama's compat layer may behave
// this way, and a missing token count is not a failed turn.
func TestOpenAIResponderToleratesMissingUsageFrame(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameFinish, frameDone)
	r := newTestLLM(srv.URL)

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(string) error { return nil })
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("tokens = (%d, %d), want (0, 0)", usage.InputTokens, usage.OutputTokens)
	}
}

// Partial usage must survive an error: finish_reason and usage arrive in
// separate frames, so a stream that dies after them has still reported them.
// This is what the Usage doc comment in responder.go requires.
func TestOpenAIResponderKeepsUsageOnMidStreamFailure(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameFinish, frameUsage, `data: {"choices":[`)
	r := newTestLLM(srv.URL)

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(string) error { return nil })
	if err == nil {
		t.Fatal("Stream() error = nil, want a decode error")
	}
	if usage.OutputTokens != 298 {
		t.Errorf("OutputTokens = %d, want 298 — usage already reported must not be discarded", usage.OutputTokens)
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
}

func TestOpenAIResponderRequestShape(t *testing.T) {
	type capturedMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type captured struct {
		Model         string            `json:"model"`
		Stream        bool              `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []capturedMessage `json:"messages"`
	}

	var body captured
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frameDone+"\n\n")
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	req := &chatpb.ChatRequest{Messages: []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "first"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "reply"},
		{Role: chatpb.Role_ROLE_USER, Content: "second"},
	}}
	if _, err := r.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	if method != http.MethodPost || path != "/chat/completions" {
		t.Errorf("request = %s %s, want POST /chat/completions", method, path)
	}
	if body.Model != "llama3.2:3b" {
		t.Errorf("model = %q, want %q", body.Model, "llama3.2:3b")
	}
	if !body.Stream {
		t.Error("stream = false, want true — a non-streaming request blocks until the whole reply is generated")
	}
	if !body.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage = false, want true — without it there are no token counts")
	}
	// An empty APIKey must send no header at all: a local Ollama needs none,
	// and "Bearer " with nothing after it is a malformed credential.
	if auth != "" {
		t.Errorf("Authorization = %q, want it absent when APIKey is empty", auth)
	}
	want := []capturedMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	if !slices.Equal(body.Messages, want) {
		t.Errorf("messages = %+v, want %+v", body.Messages, want)
	}
}

func TestOpenAIResponderSendsBearerTokenWhenSet(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frameDone+"\n\n")
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	r.APIKey = "gsk_secret"

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	if _, err := r.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if auth != "Bearer gsk_secret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer gsk_secret")
	}
}

func TestOpenAIResponderPropagatesEmitError(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameLo, frameFinish, frameDone)
	r := newTestLLM(srv.URL)
	sentinel := errors.New("send failed")

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stream() error = %v, want %v unwrapped", err, sentinel)
	}
}

// A hung-up browser must surface as a bare context.Canceled, not a gRPC
// status: classifyOutcome (chat.go) uses errors.Is/status.Code to treat a
// cancellation as expected rather than a fault, and status.Errorf's %v would
// destroy the chain that check relies on. The server here writes one delta,
// flushes, then blocks with no further frames — the client cancels from
// inside emit, mimicking a browser disconnecting mid-generation.
func TestOpenAIResponderCancelMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frameHel+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stay open with no further frames until the client hangs up
	}))
	t.Cleanup(srv.Close)
	r := newTestLLM(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(ctx, req, func(string) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	if got := classifyOutcome(err); got != "cancelled" {
		t.Errorf("classifyOutcome(err) = %q, want %q", got, "cancelled")
	}
}

// A stream that ends without [DONE] must still report whatever usage it saw
// before dying — this closes the gap TestOpenAIResponderKeepsUsageOnMidStreamFailure
// leaves open, since that test reaches its error via json.Unmarshal rather
// than the missing-sentinel path.
func TestOpenAIResponderMissingDoneSentinelKeepsUsage(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameFinish, frameUsage)
	r := newTestLLM(srv.URL)

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(string) error { return nil })
	if err == nil {
		t.Fatal("Stream() error = nil, want an error for a stream missing [DONE]")
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
	if usage.InputTokens != 26 {
		t.Errorf("InputTokens = %d, want 26", usage.InputTokens)
	}
	if usage.OutputTokens != 298 {
		t.Errorf("OutputTokens = %d, want 298", usage.OutputTokens)
	}
}

// The SSE grammar makes the space after "data:" optional; a self-hosted
// gateway in front of an OpenAI-compatible provider may omit it even though
// Ollama and OpenAI both send it.
func TestOpenAIResponderToleratesNoSpaceAfterColon(t *testing.T) {
	srv := sseServer(t, http.StatusOK,
		`data:{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`data:[DONE]`,
	)
	r := newTestLLM(srv.URL)

	var got []string
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if want := []string{"Hel"}; !slices.Equal(got, want) {
		t.Errorf("deltas = %q, want %q", got, want)
	}
}

// hangingSSEServer streams the given frames and then blocks until the client
// goes away, so a test can cancel mid-stream while the body is still open.
func hangingSSEServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			io.WriteString(w, f+"\n\n")
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIResponderProviderErrors(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    func(t *testing.T) string
		wantCode   codes.Code
		wantReason string
		wantInMsg  string
	}{
		{
			name: "provider not running",
			baseURL: func(t *testing.T) string {
				// A server closed before the request: the dial fails exactly
				// as it does when the ollama container is down.
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := srv.URL
				srv.Close()
				return url
			},
			wantCode:   codes.Unavailable,
			wantReason: "unreachable",
			wantInMsg:  "unreachable",
		},
		{
			name: "model not available",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusNotFound).URL
			},
			wantCode:   codes.Unavailable,
			wantReason: "model_missing",
			// The most likely local misconfiguration, so the message names the fix.
			wantInMsg: "ollama pull llama3.2:3b",
		},
		{
			name: "bad api key",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusUnauthorized).URL
			},
			// Internal, not Unauthenticated: the browser's credentials are not
			// the problem, our LLM_API_KEY is.
			wantCode:   codes.Internal,
			wantReason: "auth_error",
			wantInMsg:  "LLM_API_KEY",
		},
		{
			name: "rate limited",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusTooManyRequests).URL
			},
			wantCode:   codes.Unavailable,
			wantReason: "rate_limited",
			wantInMsg:  "rate limit",
		},
		{
			name: "other non-200",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusInternalServerError).URL
			},
			wantCode:   codes.Unavailable,
			wantReason: "http_error",
			wantInMsg:  "500",
		},
		{
			name: "malformed frame json",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusOK, frameHel, `data: {"choices":[`).URL
			},
			wantCode:   codes.Internal,
			wantReason: "decode_error",
			wantInMsg:  "decoding completions frame",
		},
		{
			name: "stream ends without a done sentinel",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusOK, frameHel, frameLo).URL
			},
			wantCode:   codes.Internal,
			wantReason: "decode_error",
			wantInMsg:  "without a [DONE] sentinel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := counterValue(chatProviderErrorsTotal.WithLabelValues(tt.wantReason))

			r := newTestLLM(tt.baseURL(t))
			req := &chatpb.ChatRequest{Messages: userHistory("hi")}
			_, err := r.Stream(context.Background(), req, func(string) error { return nil })

			if status.Code(err) != tt.wantCode {
				t.Fatalf("Stream() code = %v, want %v (err = %v)", status.Code(err), tt.wantCode, err)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantInMsg)
			}
			if got := counterValue(chatProviderErrorsTotal.WithLabelValues(tt.wantReason)); got != before+1 {
				t.Errorf("chat_provider_errors_total{reason=%q} = %v, want %v", tt.wantReason, got, before+1)
			}
		})
	}
}

// A hosted provider explains itself in the response body. Losing that text
// turns a one-line fix into a debugging session, so it must reach the status.
func TestOpenAIResponderSurfacesProviderErrorText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error"}}`)
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(string) error { return nil })

	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Stream() code = %v, want Unavailable (err = %v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "maximum context length") {
		t.Errorf("error = %q, want it to quote the provider's message", err.Error())
	}
}

// A body that is not OpenAI-shaped still has to come through — Ollama returns
// plain text, and truncating to nothing would be worse than passing it along.
func TestOpenAIResponderSurfacesNonJSONErrorText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "llama runner process has terminated\n")
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(string) error { return nil })

	if !strings.Contains(err.Error(), "llama runner process has terminated") {
		t.Errorf("error = %q, want it to include the raw body", err.Error())
	}
}

func TestOpenAIResponderStopsOnContextCancel(t *testing.T) {
	srv := hangingSSEServer(t, frameHel, frameLo, frameFinish, frameDone)
	r := newTestLLM(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A hung-up browser is not a provider fault, so no reason label may move.
	beforeUnreachable := counterValue(chatProviderErrorsTotal.WithLabelValues("unreachable"))
	beforeDecode := counterValue(chatProviderErrorsTotal.WithLabelValues("decode_error"))

	count := 0
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(ctx, req, func(string) error {
		count++
		cancel()
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled unwrapped so classifyOutcome sees it", err)
	}
	if count != 1 {
		t.Errorf("emitted %d deltas, want 1 — the loop must check ctx before handling the next frame", count)
	}
	if got := counterValue(chatProviderErrorsTotal.WithLabelValues("unreachable")); got != beforeUnreachable {
		t.Errorf("unreachable counter moved on cancel: %v, want %v", got, beforeUnreachable)
	}
	if got := counterValue(chatProviderErrorsTotal.WithLabelValues("decode_error")); got != beforeDecode {
		t.Errorf("decode_error counter moved on cancel: %v, want %v", got, beforeDecode)
	}
}
