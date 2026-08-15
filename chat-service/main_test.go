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
			// Set explicitly empty rather than left unset: envOr treats an
			// empty value as absent, and this keeps the case honest on a
			// machine that exports RESPONDER in its shell.
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
				// The trailing slash is trimmed, or request paths become
				// //chat/completions.
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
			// A silent fallback to echo would make a misconfigured demo look
			// like a working model.
			name:    "an unknown RESPONDER is a startup error, never a fallback",
			env:     map[string]string{"RESPONDER": "ollama"},
			wantErr: true,
		},
		{
			name:    "a malformed LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "http://[::1"},
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

// TestNewLLMClientTimeout guards the one thing standing between a wedged
// provider and a permanently pinned goroutine: newLLMClient must set a
// ResponseHeaderTimeout on its transport, and must NOT set an overall
// Client.Timeout, because a whole-request timeout would kill long
// generations.
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
