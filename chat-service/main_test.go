package main

import (
	"net/http"
	"testing"
	"time"
)

func TestNewResponder(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, r Responder)
	}{
		{
			name: "defaults to echo so an empty .env still boots",
			// Explicitly empty, not unset: envOr treats both as absent, and
			// this survives a shell that exports RESPONDER.
			env: map[string]string{"RESPONDER": ""},
			check: func(t *testing.T, r Responder) {
				if _, ok := r.(*EchoResponder); !ok {
					t.Errorf("responder = %T, want *EchoResponder", r)
				}
			},
		},
		{
			name: "llm uses the documented defaults",
			env:  map[string]string{"RESPONDER": "llm"},
			check: func(t *testing.T, r Responder) {
				o, ok := r.(*OpenAIResponder)
				if !ok {
					t.Fatalf("responder = %T, want *OpenAIResponder", r)
				}
				if o.BaseURL != defaultLLMBaseURL {
					t.Errorf("BaseURL = %q, want %q", o.BaseURL, defaultLLMBaseURL)
				}
				if o.Model != defaultLLMModel {
					t.Errorf("Model = %q, want %q", o.Model, defaultLLMModel)
				}
				if o.APIKey != "" {
					t.Errorf("APIKey = %q, want empty — a local Ollama needs no credential", o.APIKey)
				}
				if o.Client == nil {
					t.Error("Client = nil, want a client with a response-header timeout")
				}
			},
		},
		{
			name: "llm honours base url, model and api key",
			env: map[string]string{
				"RESPONDER":    "llm",
				"LLM_BASE_URL": "https://api.groq.com/openai/v1/",
				"LLM_MODEL":    "llama-3.3-70b-versatile",
				"LLM_API_KEY":  "gsk_secret",
			},
			check: func(t *testing.T, r Responder) {
				o := r.(*OpenAIResponder)
				// Trimmed, or request paths become //chat/completions.
				if o.BaseURL != "https://api.groq.com/openai/v1" {
					t.Errorf("BaseURL = %q, want %q", o.BaseURL, "https://api.groq.com/openai/v1")
				}
				if o.Model != "llama-3.3-70b-versatile" {
					t.Errorf("Model = %q, want %q", o.Model, "llama-3.3-70b-versatile")
				}
				if o.APIKey != "gsk_secret" {
					t.Errorf("APIKey = %q, want %q", o.APIKey, "gsk_secret")
				}
			},
		},
		{
			// A silent fallback would make a misconfigured demo look working.
			name:    "an unknown RESPONDER is a startup error, never a fallback",
			env:     map[string]string{"RESPONDER": "ollama"},
			wantErr: true,
		},
		{
			name:    "a malformed LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "http://[::1"},
			wantErr: true,
		},
		{
			// url.Parse accepts a bare host as an opaque URL, so only a scheme
			// check catches this. Otherwise the service boots healthy and fails
			// every turn as reason="unreachable".
			name:    "a scheme-less LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "ollama:11434/v1"},
			wantErr: true,
		},
		{
			name:    "a non-URL LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "not a url at all"},
			wantErr: true,
		},
		{
			name:    "a hostless LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "http:///v1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			r, err := newResponder()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newResponder() error = nil, want an error (got %T)", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("newResponder() error = %v, want nil", err)
			}
			tt.check(t, r)
		})
	}
}

// newLLMClient must set ResponseHeaderTimeout — the only thing between a
// wedged provider and a pinned goroutine — and must NOT set Client.Timeout,
// which would kill long generations.
func TestNewLLMClientTimeout(t *testing.T) {
	client := newLLMClient()

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, 120*time.Second)
	}
	if client.Timeout != 0 {
		t.Errorf("client.Timeout = %v, want 0 — a whole-request timeout would kill long generations", client.Timeout)
	}
}
