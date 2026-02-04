# Testing & runbook

Quick validation steps

1. Verify GPU and drivers on inference node

```bash
nvidia-smi
uname -a
docker --version || podman --version
```

2. Start Ollama container (example)

```bash
docker run -d --name ollama -p 11434:11434 -v /opt/ollama/models:/models --gpus all ollama/ollama:latest
```

3. Observe GPU usage

```bash
nvidia-smi -l 1
```

4. Health check Ollama (adjust per Ollama API)

```bash
curl -sS http://<inference-host>:11434/health
```

5. Test adapter

```bash
curl -X POST http://<adapter-host>:8080/v1/chat/completions \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"local-7b","messages":[{"role":"user","content":"Say hello"}]}'
```

6. Load test (light)

- Use `wrk` or `vegeta` to send a small burst to evaluate latency and concurrency.

Example `vegeta` quick test

```bash
echo '{"method":"POST","url":"http://<adapter-host>:8080/v1/chat/completions","body":"{\"model\":\"local-7b\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"}' > target.json
vegeta attack -targets=target.json -rate=2 -duration=30s | vegeta report
```

Monitoring & logging

- Export Ollama and adapter logs to centralized logging (ELK/Promtail+Loki).
- Create a Prometheus scrape for resource metrics and basic health endpoints.

Troubleshooting

- "Model fails to load": check Ollama logs and `nvidia-smi` for OOM; try smaller quantization variant.
- "Adapter returns 401": confirm `AUTH_TOKEN` and header are correct.
- "High latency": measure CPU/GPU utilization, reduce batch size or sequence length, consider CPU offload.

Security

- Restrict access to `ollama` service with NetworkPolicy and Tailscale ACLs.
- Use TLS termination at an ingress or with a sidecar (Caddy/Nginx) and require bearer tokens.

If you want, I can: build the adapter image here, render the final YAML files with your registry values, or produce a small conversion script that runs on a worker node.