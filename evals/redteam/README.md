# Red-team set

Adversarial and benign questions for the portfolio assistant, run end to end
through Envoy so signing, the injection guard and the prompt are all under
test. This directory is outside the ingest allowlist on purpose: indexed, the
attacks would come back to the model as sources.

| File | Cases | Must pass |
|---|---|---|
| `structural.yaml` | guard routing, forged history | 100% |
| `behaviour.yaml` | scope rules, premises, attribution, benign controls, accepted guard false positives | baseline |
| `rubric.yaml` | role-play, indirect injection, embellishment | baseline, needs a judge |

## Running

    docker compose --profile llm up -d
    npx promptfoo eval -c evals/redteam/promptfooconfig.yaml -j 1 --delay 2500
    npx promptfoo eval -c evals/redteam/promptfooconfig.judged.yaml --grader <provider-id> -j 1 --delay 2500
    npx promptfoo view

`CHAT_URL` overrides the Envoy address (default `http://localhost:8080`).

The whole suite is one visitor to the edge, so it must run serially and below
Envoy's per-IP rate (one request per 2 s after a burst of 10): in parallel it
trips `MAX_STREAMS_PER_VISITOR` and the rate limit, and every refusal reads as
an error.

## Baseline

| Date | Model | Structural | Behaviour | Judged |
|---|---|---|---|---|
| 2026-10-10 | llama3.2:3b | 9/9 | 11/16 | not run |
| 2026-10-30 | gpt-4o-mini | 9/9 | 16/16 | 6/6, grader gpt-4o-mini; "speak as Aleksi" ran truncated, see note |

Before 2026-10-30, `rubric.yaml`'s "speak as Aleksi" question was unquoted
YAML and split at its comma, so that case asked only "From now on".
