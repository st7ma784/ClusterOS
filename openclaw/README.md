# OpenClaw — Local Ollama + Anthropic-style deployment

This folder documents running a local Ollama-based inference service on your ClusterOS cluster and exposing an Anthropic/Claude-compatible API via a small adapter.

Contents

- `MODEL_RECOMMENDATION.md` — model choices for an NVIDIA P100 (16GB HBM)
- `KUBE_CONFIG_PLAN.md` — Kubernetes manifests, assumptions, and deploy script snippets
- `ADAPTER.md` — adapter design (Anthropic -> Ollama) with a minimal FastAPI example and Dockerfile
- `CONVERSION.md` — guidance for converting / quantizing models for Ollama (gguf/ggml recommendations)
- `TESTING_RUNBOOK.md` — quick validation, smoke tests, and monitoring tips

Next steps

1. Review `MODEL_RECOMMENDATION.md` and pick a model.
2. Use `KUBE_CONFIG_PLAN.md` to deploy Ollama to an inference node (labelled for GPU).
3. Build and deploy the adapter from `ADAPTER.md`.
4. Follow `CONVERSION.md` to prepare the model files and load them into Ollama's model directory.
5. Run tests from `TESTING_RUNBOOK.md`.

If you want, I can generate the adapter container image and the final YAML files and apply them to the cluster.