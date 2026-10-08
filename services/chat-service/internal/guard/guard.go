// Package guard screens visitor text for prompt injection with a classifier.
// A classifier returns a score, so unlike an LLM judge it cannot be told what
// to answer.
package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Verdict is the worst window across every text in one Check.
type Verdict struct {
	Flagged bool
	Score   float32
}

type Guard interface {
	Check(ctx context.Context, texts []string) (Verdict, error)
}

// Windows are budgeted in bytes because no token covers less than one, so even
// byte-fallback text stays under Prompt Guard's 512 tokens. They overlap by
// half so an attack straddling a boundary is whole in one.
const (
	windowBytes = 500
	strideBytes = 250
	// TEI's default --max-client-batch-size.
	maxBatch     = 32
	checkTimeout = 5 * time.Second
)

const (
	maliciousLabel = "MALICIOUS"
	benignLabel    = "BENIGN"
)

// TEIGuard talks to text-embeddings-inference serving Prompt Guard 2.
type TEIGuard struct {
	BaseURL   string // no trailing slash
	Threshold float32
	HTTP      *http.Client
}

// Check records exactly one verdict, except for a caller hang-up, which is
// not a guard failure.
func (g *TEIGuard) Check(ctx context.Context, texts []string) (Verdict, error) {
	start := time.Now()
	v, err := g.check(ctx, texts)
	chatGuardDuration.Observe(time.Since(start).Seconds())
	switch {
	case err != nil && ctx.Err() != nil:
		return v, ctx.Err()
	case err != nil:
		chatGuardChecksTotal.WithLabelValues("error").Inc()
	case v.Flagged:
		chatGuardChecksTotal.WithLabelValues("malicious").Inc()
	default:
		chatGuardChecksTotal.WithLabelValues("benign").Inc()
	}
	return v, err
}

func (g *TEIGuard) check(ctx context.Context, texts []string) (Verdict, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	var inputs []string
	for _, t := range texts {
		inputs = append(inputs, windows(t)...)
	}
	var worst float32
	for start := 0; start < len(inputs); start += maxBatch {
		scores, err := g.predict(ctx, inputs[start:min(start+maxBatch, len(inputs))])
		if err != nil {
			return Verdict{}, err
		}
		for _, s := range scores {
			worst = max(worst, s)
		}
	}
	chatGuardScore.Observe(float64(worst))
	return Verdict{Flagged: worst >= g.Threshold, Score: worst}, nil
}

// windows cuts only on rune boundaries: a split character is invalid UTF-8,
// and TEI rejects the whole batch over one invalid input.
func windows(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for start := 0; ; {
		end := min(start+windowBytes, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		out = append(out, text[start:end])
		if end == len(text) {
			return out
		}
		start += strideBytes
		for !utf8.RuneStart(text[start]) {
			start--
		}
	}
}

type prediction struct {
	Label string  `json:"label"`
	Score float32 `json:"score"`
}

func (g *TEIGuard) predict(ctx context.Context, inputs []string) ([]float32, error) {
	req := struct {
		Inputs   [][]string `json:"inputs"`
		Truncate bool       `json:"truncate"`
	}{Truncate: true}
	for _, in := range inputs {
		req.Inputs = append(req.Inputs, []string{in})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/predict", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.HTTP.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("guard request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("guard returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var out [][]prediction
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding guard response: %w", err)
	}
	if len(out) != len(inputs) {
		return nil, fmt.Errorf("guard scored %d of %d inputs", len(out), len(inputs))
	}
	scores := make([]float32, len(out))
	for i, preds := range out {
		if scores[i], err = maliciousScore(preds); err != nil {
			return nil, err
		}
	}
	return scores, nil
}

// maliciousScore errors on unknown labels: reading them as 0 would turn a
// model swap into a guard that passes everything.
func maliciousScore(preds []prediction) (float32, error) {
	for _, p := range preds {
		if strings.EqualFold(p.Label, maliciousLabel) {
			return p.Score, nil
		}
	}
	for _, p := range preds {
		if strings.EqualFold(p.Label, benignLabel) {
			return 1 - p.Score, nil
		}
	}
	return 0, fmt.Errorf("guard response has neither %s nor %s: %+v", maliciousLabel, benignLabel, preds)
}

// Ping succeeds once TEI has loaded the model; /health fails before that.
func (g *TEIGuard) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("guard /health returned HTTP %d", resp.StatusCode)
	}
	return nil
}
