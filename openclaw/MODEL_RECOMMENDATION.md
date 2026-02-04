# Model recommendation for NVIDIA P100 (16GB HBM)

Summary

- Target GPU: NVIDIA Tesla P100 (typically 16GB HBM). Small-ish VRAM compared with modern GPUs, so prefer 7B-class models with 4-bit quantization.
- Recommended models:
  - `Llama-2-7B` (instruction / chat variant) — well-known, instruction-tuned, fits comfortably when quantized to 4-bit (Q4) gguf/ggml.
  - `Mistral-7B` (instruct) — strong instruction-following performance and often similar VRAM profile when quantized.
- Avoid 13B+ models unless you accept CPU offload, sharding, or much slower latency.

Quantization and formats

- Use 4-bit quantization (Q4) to fit in 16GB VRAM; Q4_K_M or other Q4 variants depending on conversion tool.
- Preferred target formats for Ollama: gguf/ggml variants or whatever Ollama accepts as model artifacts (confirm with your Ollama version). If Ollama accepts local gguf models, place them in the mounted models directory.

Estimated VRAM usage (approximate)

- Llama-2-7B (Q4): ~6–12GB VRAM depending on sequence length, tokenizer, and batch size.
- Mistral-7B (Q4): similar range; test to confirm.

What to measure

- Peak GPU memory while loading model: `nvidia-smi -l 1` and start the server.
- CPU usage and disk I/O when quantizing or loading models.

Fallbacks and CPU-only

- If no GPU, use a quantized CPU execution path (slower) and pick the smallest quantized model possible. Expect significant latency for large models on CPU.

Model behaviour tuning for Anthropic-style

- Use instruction-tuned/chat variants where possible.
- Add a small prompt template layer in the adapter to emulate Anthropic system prompts and safety scaffolding.

Licensing

- Verify model license before hosting locally (some weights have restrictions). Always confirm redistributability and usage rights for local hosting.

Quick test approach

1. Download/convert model (see `CONVERSION.md`).
2. Start Ollama pointing to the model directory.
3. Observe `nvidia-smi` to check peak memory and adjust quantization/batching accordingly.

If you want I can pick one model (e.g., `Llama-2-7b-chat`), produce exact conversion commands, and prepare a test container manifest for you.