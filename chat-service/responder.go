package main

import (
	"context"
	"strings"
	"time"

	chatpb "chat-service/chat"
)

// Usage reports what a Responder consumed and produced for one turn. The echo
// stub leaves InputTokens at zero.
//
// Usage may be partially populated even when Stream returns an error: a
// provider that fails mid-stream still billed for what it emitted. Callers
// must not discard it on error.
type Usage struct {
	StopReason   string
	InputTokens  int32
	OutputTokens int32
}

// Responder produces an assistant reply, emitting it in pieces. emit is called
// once per piece; if it returns an error, Stream stops and returns it
// unchanged.
//
// This is the seam for a new provider — the RPC handler does not change. It
// takes the full request rather than a history slice so future ChatRequest
// fields (model, temperature, system prompt) need no interface change.
type Responder interface {
	Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error)
}

// EchoResponder replays the last user message one word at a time. It stands in
// for a real model so the streaming transport can be exercised at zero cost.
type EchoResponder struct {
	// Delay is the pause before each word, imitating model latency.
	Delay time.Duration
}

func (e *EchoResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	words := strings.Fields(lastContent(req.GetMessages()))

	for i, word := range words {
		select {
		case <-ctx.Done():
			return Usage{}, ctx.Err()
		case <-time.After(e.Delay):
		}

		// The client concatenates deltas verbatim, so the separator has to
		// travel with the word.
		delta := word
		if i > 0 {
			delta = " " + word
		}
		if err := emit(delta); err != nil {
			return Usage{}, err
		}
	}

	return Usage{
		StopReason:   "end_turn",
		OutputTokens: int32(len(words)),
	}, nil
}

func lastContent(history []*chatpb.Message) string {
	if len(history) == 0 {
		return ""
	}
	return history[len(history)-1].GetContent()
}
