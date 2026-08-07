package main

import (
	"context"
	"strings"
	"time"

	chatpb "chat-service/chat"
)

// Usage reports what a Responder consumed and produced for one turn.
// The echo stub leaves InputTokens at zero; a model-backed Responder fills
// all three fields from the provider's usage report.
type Usage struct {
	StopReason   string
	InputTokens  int32
	OutputTokens int32
}

// Responder produces an assistant reply for a conversation, emitting it in
// pieces as they become available. emit is called once per piece; if emit
// returns an error, Stream stops and returns that error unchanged.
//
// This is the seam for roadmap step 3: a Claude-backed implementation drops
// in here without the RPC handler changing.
type Responder interface {
	Stream(ctx context.Context, history []*chatpb.Message, emit func(delta string) error) (Usage, error)
}

// EchoResponder replays the last user message one word at a time. It stands in
// for a real model so the streaming transport can be exercised at zero cost.
type EchoResponder struct {
	// Delay is the pause before each word, imitating model latency.
	Delay time.Duration
}

func (e *EchoResponder) Stream(ctx context.Context, history []*chatpb.Message, emit func(delta string) error) (Usage, error) {
	words := strings.Fields(lastContent(history))

	for i, word := range words {
		select {
		case <-ctx.Done():
			return Usage{}, ctx.Err()
		case <-time.After(e.Delay):
		}

		// Re-join with single spaces: the client concatenates deltas verbatim,
		// so the separator has to travel with the word.
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
