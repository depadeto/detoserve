"""
DetoServe API Gateway — single consumer-facing endpoint

Consumers hit ONE endpoint:
    POST http://gateway:8090/v1/chat/completions  (body: {"model": "llama-70b-chat", ...})
    GET  http://gateway:8090/v1/models

The gateway:
  1. Reads the "model" field from the request
  2. Looks up all running instances of that model from the Function Manager
  3. Load-balances across them (round-robin, with future KV-cache-aware routing)
  4. Proxies the request to the selected backend endpoint

For multi-cluster: each instance has an endpoint reachable from this gateway.
In production, this sits behind Envoy (see gateway.yaml) with TLS + auth.
"""

import asyncio
import itertools
import os
import time
from typing import Optional

import httpx
from fastapi import FastAPI, Request, HTTPException, Header
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import StreamingResponse, JSONResponse

app = FastAPI(title="DetoServe API Gateway", version="0.1.0")

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)

FUNCTION_MANAGER_URL = os.getenv("FUNCTION_MANAGER_URL", "http://localhost:8086")
GATEWAY_PORT = int(os.getenv("GATEWAY_PORT", "8090"))

# Backend registry: model_name -> list of endpoints, refreshed periodically
_backends: dict[str, list[dict]] = {}
_round_robin: dict[str, itertools.cycle] = {}
_last_refresh = 0.0
REFRESH_INTERVAL = 5  # seconds


async def _refresh_backends():
    """Pull running instances from Function Manager and build routing table."""
    global _backends, _round_robin, _last_refresh

    if time.time() - _last_refresh < REFRESH_INTERVAL:
        return

    try:
        async with httpx.AsyncClient(timeout=5) as client:
            fn_resp = await client.get(f"{FUNCTION_MANAGER_URL}/api/functions")
            fn_resp.raise_for_status()
            functions = {f["id"]: f for f in fn_resp.json()}

            inst_resp = await client.get(f"{FUNCTION_MANAGER_URL}/api/instances")
            inst_resp.raise_for_status()
            instances = inst_resp.json()

        new_backends: dict[str, list[dict]] = {}
        for inst in instances:
            if inst.get("status") != "running":
                continue
            endpoint = inst.get("endpoint", "")
            if not endpoint:
                continue

            fn = functions.get(inst.get("function_id", ""))
            if not fn:
                continue

            model_name = fn.get("name", "")
            if not model_name:
                continue

            backend = {
                "instance_id": inst["id"],
                "endpoint": endpoint,
                "tenant_id": inst.get("tenant_id", ""),
                "cluster": inst.get("cluster", ""),
                "sky_cluster": inst.get("sky_cluster_name", ""),
                "gpu_type": fn.get("resources", {}).get("gpu_type", ""),
                "gpu_count": fn.get("resources", {}).get("gpu_count", 0),
            }

            if model_name not in new_backends:
                new_backends[model_name] = []
            new_backends[model_name].append(backend)

        _backends = new_backends
        _round_robin = {
            name: itertools.cycle(backends)
            for name, backends in _backends.items()
        }
        _last_refresh = time.time()

    except Exception as e:
        print(f"[gateway] Failed to refresh backends: {e}")


def _select_backend(model: str, tenant_id: Optional[str] = None) -> dict:
    """Pick a backend for the given model. Round-robin for now.

    Future: KV-cache-aware routing, session affinity, latency-based.
    """
    backends = _backends.get(model)
    if not backends:
        return {}

    if tenant_id:
        tenant_backends = [b for b in backends if b["tenant_id"] == tenant_id]
        if tenant_backends:
            rr_key = f"{model}:{tenant_id}"
            if rr_key not in _round_robin:
                _round_robin[rr_key] = itertools.cycle(tenant_backends)
            return next(_round_robin[rr_key])

    rr = _round_robin.get(model)
    if not rr:
        return {}
    return next(rr)


# --- Consumer-facing endpoints ---

@app.get("/v1/models")
async def list_models():
    """OpenAI-compatible: list available models."""
    await _refresh_backends()
    models = []
    for model_name, backends in _backends.items():
        models.append({
            "id": model_name,
            "object": "model",
            "owned_by": "detoserve",
            "endpoints": len(backends),
        })
    return {"object": "list", "data": models}


@app.post("/v1/chat/completions")
@app.post("/v1/completions")
async def proxy_inference(request: Request, x_tenant_id: Optional[str] = Header(None)):
    """Proxy inference requests to the right backend."""
    await _refresh_backends()

    body = await request.json()
    model = body.get("model", "")

    if not model:
        raise HTTPException(400, "missing 'model' field in request body")

    backend = _select_backend(model, tenant_id=x_tenant_id)
    if not backend:
        available = list(_backends.keys())
        raise HTTPException(
            404,
            f"no running instances for model '{model}'. "
            f"Available models: {available}"
        )

    target_url = f"{backend['endpoint']}/v1/chat/completions"

    try:
        async with httpx.AsyncClient(timeout=120) as client:
            resp = await client.post(
                target_url,
                json=body,
                headers={
                    "Content-Type": "application/json",
                    "X-DetoServe-Instance": backend["instance_id"],
                    "X-DetoServe-Cluster": backend["cluster"],
                },
            )
            return JSONResponse(
                content=resp.json(),
                status_code=resp.status_code,
                headers={
                    "X-DetoServe-Instance": backend["instance_id"],
                    "X-DetoServe-Cluster": backend["cluster"],
                    "X-DetoServe-Backend": backend["endpoint"],
                },
            )
    except httpx.ConnectError:
        raise HTTPException(
            502,
            f"cannot reach backend {backend['endpoint']} "
            f"(cluster: {backend['cluster']})"
        )
    except Exception as e:
        raise HTTPException(502, f"proxy error: {str(e)}")


@app.api_route("/v1/{path:path}", methods=["GET", "POST", "PUT", "DELETE"])
async def proxy_catchall(path: str, request: Request, x_tenant_id: Optional[str] = Header(None)):
    """Catch-all proxy for any /v1/* path — routes to correct model backend."""
    await _refresh_backends()

    model = None
    if request.method in ("POST", "PUT"):
        try:
            body = await request.json()
            model = body.get("model", "")
        except Exception:
            body = {}
    else:
        model = request.query_params.get("model", "")
        body = None

    if not model and len(_backends) == 1:
        model = list(_backends.keys())[0]

    if not model:
        raise HTTPException(400, "specify 'model' param or field")

    backend = _select_backend(model, tenant_id=x_tenant_id)
    if not backend:
        raise HTTPException(404, f"no instances for model '{model}'")

    target = f"{backend['endpoint']}/v1/{path}"
    try:
        async with httpx.AsyncClient(timeout=30) as client:
            if request.method == "GET":
                resp = await client.get(target)
            else:
                resp = await client.post(target, json=body)
            return JSONResponse(
                content=resp.json(),
                status_code=resp.status_code,
                headers={
                    "X-DetoServe-Instance": backend["instance_id"],
                    "X-DetoServe-Cluster": backend["cluster"],
                },
            )
    except Exception as e:
        raise HTTPException(502, f"proxy error: {str(e)}")


# --- Management / health ---

@app.get("/healthz")
async def health():
    await _refresh_backends()
    total_backends = sum(len(b) for b in _backends.values())
    return {
        "status": "ok",
        "models": len(_backends),
        "total_backends": total_backends,
        "routing_table": {
            name: [
                {
                    "instance": b["instance_id"],
                    "cluster": b["cluster"],
                    "endpoint": b["endpoint"],
                }
                for b in backends
            ]
            for name, backends in _backends.items()
        },
    }


@app.get("/")
async def root():
    await _refresh_backends()
    models = list(_backends.keys())
    return {
        "service": "DetoServe API Gateway",
        "version": "0.1.0",
        "usage": {
            "endpoint": f"http://localhost:{GATEWAY_PORT}/v1/chat/completions",
            "method": "POST",
            "body": {"model": "<model-name>", "messages": [{"role": "user", "content": "Hello"}]},
            "headers": {"X-Tenant-ID": "(optional) your tenant ID"},
        },
        "available_models": models,
        "models_endpoint": f"http://localhost:{GATEWAY_PORT}/v1/models",
    }


if __name__ == "__main__":
    import uvicorn
    print(f"DetoServe API Gateway starting on :{GATEWAY_PORT}")
    print(f"  Consumer endpoint: http://localhost:{GATEWAY_PORT}/v1/chat/completions")
    print(f"  Models:            http://localhost:{GATEWAY_PORT}/v1/models")
    print(f"  Function Manager:  {FUNCTION_MANAGER_URL}")
    uvicorn.run(app, host="0.0.0.0", port=GATEWAY_PORT)
