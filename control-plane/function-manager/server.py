"""
Function Manager — Python FastAPI equivalent of main.go

A "Function" is a reusable template: define once, deploy many times.
An "Instance" is a running deployment of that function on a specific cluster.
"""

import os
import time
import threading
from datetime import datetime, timezone
from typing import Optional

from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel, Field

app = FastAPI(title="Function Manager", version="0.1.0")

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)


# --- Models ---

class ResourceSpec(BaseModel):
    gpu_type: str = "A100"
    gpu_count: int = 1
    tensor_parallel: int = 0
    max_model_len: int = 8192
    cpu: str = ""
    memory: str = ""

class ScalingSpec(BaseModel):
    min_replicas: int = 1
    max_replicas: int = 10
    metric: str = "queue_depth"
    target_value: int = 10

class RoutingSpec(BaseModel):
    prefix_caching: bool = True
    session_affinity: bool = False

class Function(BaseModel):
    id: str = ""
    name: str = ""
    description: str = ""
    version: str = "v1"
    runtime: str = "vllm"
    model_uri: str = ""
    model_format: str = ""
    quantization: str = ""
    resources: ResourceSpec = Field(default_factory=ResourceSpec)
    scaling: ScalingSpec = Field(default_factory=ScalingSpec)
    routing: RoutingSpec = Field(default_factory=RoutingSpec)
    extra_args: list[str] = []
    env_vars: dict[str, str] = {}
    tags: dict[str, str] = {}
    created_at: str = ""
    updated_at: str = ""
    created_by: str = ""

class Instance(BaseModel):
    id: str = ""
    function_id: str = ""
    function_name: str = ""
    tenant_id: str = ""
    cluster: str = ""
    region: str = ""
    cloud: str = ""
    use_spot: bool = False
    status: str = "deploying"
    endpoint: str = ""
    replicas: int = 1
    created_at: str = ""
    updated_at: str = ""

class DeployRequest(BaseModel):
    tenant_id: str = ""
    cluster: str = ""
    region: str = ""
    cloud: str = ""
    use_spot: bool = False


# --- In-memory store ---

_functions: dict[str, dict] = {}
_instances: dict[str, dict] = {}
_lock = threading.Lock()


def _now():
    return datetime.now(timezone.utc).isoformat()


# --- Simulated status progression ---

def _progress_instance(inst_id: str):
    """Simulate deployment progressing from deploying -> running."""
    time.sleep(5)
    with _lock:
        if inst_id in _instances:
            _instances[inst_id]["status"] = "running"
            _instances[inst_id]["endpoint"] = f"https://api.detoserve.ai/v1/{inst_id}"
            _instances[inst_id]["updated_at"] = _now()


# --- Endpoints ---

@app.get("/healthz")
def health():
    return {"status": "ok"}


# ===== FUNCTIONS =====

@app.post("/api/functions", status_code=201)
def create_function(f: Function):
    f.id = f"fn-{int(time.time() * 1000)}"
    f.created_at = _now()
    f.updated_at = _now()
    if f.version == "":
        f.version = "v1"
    if f.resources.tensor_parallel == 0:
        f.resources.tensor_parallel = f.resources.gpu_count
    if f.scaling.min_replicas == 0:
        f.scaling.min_replicas = 1
    if f.scaling.max_replicas == 0:
        f.scaling.max_replicas = 10
    if f.scaling.metric == "":
        f.scaling.metric = "queue_depth"
    if f.scaling.target_value == 0:
        f.scaling.target_value = 10

    data = f.model_dump()
    with _lock:
        _functions[f.id] = data
    return data


@app.get("/api/functions")
def list_functions():
    with _lock:
        return list(_functions.values())


@app.get("/api/functions/{fn_id}")
def get_function(fn_id: str):
    with _lock:
        f = _functions.get(fn_id)
    if not f:
        raise HTTPException(status_code=404, detail="not found")
    return f


@app.put("/api/functions/{fn_id}")
def update_function(fn_id: str, update: Function):
    with _lock:
        f = _functions.get(fn_id)
        if not f:
            raise HTTPException(status_code=404, detail="not found")
        if update.name:
            f["name"] = update.name
        if update.description:
            f["description"] = update.description
        if update.model_uri:
            f["model_uri"] = update.model_uri
        if update.runtime:
            f["runtime"] = update.runtime
        if update.version:
            f["version"] = update.version
        f["updated_at"] = _now()
        _functions[fn_id] = f
    return f


@app.delete("/api/functions/{fn_id}")
def delete_function(fn_id: str):
    with _lock:
        _functions.pop(fn_id, None)
    return {"status": "deleted"}


# ===== INSTANCES (deploy a function) =====

@app.post("/api/functions/{fn_id}/deploy", status_code=201)
def deploy_instance(fn_id: str, req: DeployRequest):
    with _lock:
        f = _functions.get(fn_id)
    if not f:
        raise HTTPException(status_code=404, detail="function not found")

    inst = {
        "id": f"inst-{int(time.time() * 1000)}",
        "function_id": f["id"],
        "function_name": f["name"],
        "tenant_id": req.tenant_id,
        "cluster": req.cluster,
        "region": req.region,
        "cloud": req.cloud,
        "use_spot": req.use_spot,
        "status": "deploying",
        "endpoint": "",
        "replicas": f.get("scaling", {}).get("min_replicas", 1),
        "created_at": _now(),
        "updated_at": _now(),
    }

    with _lock:
        _instances[inst["id"]] = inst

    threading.Thread(target=_progress_instance, args=(inst["id"],), daemon=True).start()

    return inst


@app.get("/api/instances")
def list_instances(function_id: Optional[str] = None):
    with _lock:
        if function_id:
            return [i for i in _instances.values() if i["function_id"] == function_id]
        return list(_instances.values())


@app.get("/api/instances/{inst_id}")
def get_instance(inst_id: str):
    with _lock:
        inst = _instances.get(inst_id)
    if not inst:
        raise HTTPException(status_code=404, detail="not found")
    return inst


@app.delete("/api/instances/{inst_id}")
def delete_instance(inst_id: str):
    with _lock:
        _instances.pop(inst_id, None)
    return {"status": "deleted"}


if __name__ == "__main__":
    import uvicorn
    port = int(os.getenv("PORT", "8086"))
    print(f"Function Manager starting on :{port}")
    uvicorn.run(app, host="0.0.0.0", port=port)
