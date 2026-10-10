#!/bin/sh
# Fetches Prompt Guard 2 at a pinned revision for TEI to load from disk. Meta's
# config.json has no id2label, which TEI requires; transformers' LABEL_n default
# would not match the BENIGN/MALICIOUS that chat-service's guard client parses.
set -eu
dir="/data/prompt-guard-2-22m/$REVISION"
[ -f "$dir/.complete" ] && exit 0
: "${HF_TOKEN:?set HF_TOKEN in infra/guard/.env}"

mkdir -p "$dir"
for f in config.json tokenizer.json tokenizer_config.json model.safetensors; do
  curl -fsSL -H "Authorization: Bearer $HF_TOKEN" -o "$dir/$f" \
    "https://huggingface.co/$MODEL/resolve/$REVISION/$f"
done

grep -q '"id2label"' "$dir/config.json" ||
  sed -i 's/^{$/{"id2label": {"0": "BENIGN", "1": "MALICIOUS"}, "label2id": {"BENIGN": 0, "MALICIOUS": 1},/' "$dir/config.json"
grep -q '"MALICIOUS"' "$dir/config.json"
touch "$dir/.complete"
