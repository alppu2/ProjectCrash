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
    npx promptfoo eval -c evals/redteam/promptfooconfig.yaml
    npx promptfoo eval -c evals/redteam/promptfooconfig.judged.yaml --grader <provider-id>
    npx promptfoo view

`CHAT_URL` overrides the Envoy address (default `http://localhost:8080`).

## Baseline

| Date | Model | Structural | Behaviour | Judged |
|---|---|---|---|---|
| not yet run | llama3.2:3b | –/9 | –/16 | not run |
