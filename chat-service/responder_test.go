package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	chatpb "chat-service/chat"
)

func userHistory(contents ...string) []*chatpb.Message {
	msgs := make([]*chatpb.Message, 0, len(contents))
	for _, c := range contents {
		msgs = append(msgs, &chatpb.Message{Role: chatpb.Role_ROLE_USER, Content: c})
	}
	return msgs
}

func TestEchoResponderChunking(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"single word", "hello", []string{"hello"}},
		{"multiple words", "hello there world", []string{"hello", " there", " world"}},
		{"collapses extra whitespace", "  hello   there  ", []string{"hello", " there"}},
		{"empty content", "", nil},
		{"whitespace only", "   ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &EchoResponder{}
			var got []string

			usage, err := r.Stream(context.Background(), userHistory(tt.content), func(d string) error {
				got = append(got, d)
				return nil
			})
			if err != nil {
				t.Fatalf("Stream() error = %v, want nil", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("deltas = %q, want %q", got, tt.want)
			}
			if usage.StopReason != "end_turn" {
				t.Errorf("StopReason = %q, want %q", usage.StopReason, "end_turn")
			}
			if int(usage.OutputTokens) != len(tt.want) {
				t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, len(tt.want))
			}
		})
	}
}

func TestEchoResponderUsesLastMessage(t *testing.T) {
	r := &EchoResponder{}
	history := []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "first"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "reply"},
		{Role: chatpb.Role_ROLE_USER, Content: "second"},
	}

	var got []string
	if _, err := r.Stream(context.Background(), history, func(d string) error {
		got = append(got, d)
		return nil
	}); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	if want := []string{"second"}; !slices.Equal(got, want) {
		t.Errorf("deltas = %q, want %q", got, want)
	}
}

func TestEchoResponderStopsOnCancel(t *testing.T) {
	r := &EchoResponder{Delay: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	count := 0
	_, err := r.Stream(ctx, userHistory("one two three four five"), func(string) error {
		count++
		if count == 2 {
			cancel()
		}
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	if count != 2 {
		t.Errorf("emitted %d deltas, want 2 (should stop at cancel)", count)
	}
}

func TestEchoResponderPropagatesEmitError(t *testing.T) {
	r := &EchoResponder{}
	sentinel := errors.New("send failed")

	_, err := r.Stream(context.Background(), userHistory("one two"), func(string) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stream() error = %v, want %v", err, sentinel)
	}
}
