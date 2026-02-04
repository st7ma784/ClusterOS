# Anthropic -> Ollama Adapter

Purpose

Provide a small HTTP adapter that accepts Anthropic/Claude-style requests and translates them to Ollama's API shape. Run the adapter as a separate `Deployment` and secure it (token/TLS).

Design choices

- Language: Python + FastAPI (small, async, easy to containerize).
- Endpoint: `POST /v1/chat/completions` (or `POST /v1/complete`) matching Anthropic-style fields.
- Security: read token from `AUTH_TOKEN` env var; support TLS via an ingress or sidecar reverse proxy.
- Mapping: translate Anthropic messages/prompts to the target Ollama model input. Add a system prompt template to emulate safety.

Minimal FastAPI example

```python
from fastapi import FastAPI, Request, HTTPException
import httpx
import os

app = FastAPI()
OLLAMA_URL = os.environ.get("OLLAMA_URL", "http://ollama:11434")
AUTH_TOKEN = os.environ.get("AUTH_TOKEN", "")

@app.post("/v1/chat/completions")
async def chat_completions(req: Request):
    body = await req.json()
    # Basic auth check (bearer token)
    auth = req.headers.get("authorization", "")
    if AUTH_TOKEN and auth != f"Bearer {AUTH_TOKEN}":
        raise HTTPException(status_code=401, detail="unauthorized")

    # Extract Anthropic-style fields
    # Example mapping: messages -> prompt
    messages = body.get("messages") or []
    prompt = "\n".join([m.get("content", "") for m in messages])

    # Build Ollama request (adjust to actual Ollama API)
    ollama_payload = {"model": body.get("model", "default"), "input": prompt}

    async with httpx.AsyncClient() as client:
        r = await client.post(f"{OLLAMA_URL}/api/generate", json=ollama_payload, timeout=120)
        r.raise_for_status()
        data = r.json()

    # Map Ollama response back to Anthropic-style
    return {"id": "local-1", "object": "chat.completion", "choices": [{"message": {"role": "assistant", "content": data.get("output", "")}}]}
```

Dockerfile (minimal)

```Dockerfile
FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install -r requirements.txt
COPY adapter.py ./
CMD ["uvicorn", "adapter:app", "--host", "0.0.0.0", "--port", "8080"]
```

`requirements.txt` example

```
fastapi
httpx[http2]
uvicorn[standard]
```

Notes

- Adjust the Ollama API endpoint and payload based on your Ollama version.
- Add streaming support if you need server-sent events or chunked responses.
- Add request validation and rate limiting for safety.

I can generate a ready-to-build adapter directory (source + Dockerfile + manifest) if you want.