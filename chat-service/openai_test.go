package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

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
