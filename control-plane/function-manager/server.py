"""
Function Manager — Python FastAPI equivalent of main.go

A "Function" is a reusable template: define once, deploy many times.
An "Instance" is a running deployment of that function on a specific cluster.

When deploying, actually creates Kubernetes Deployments with GPU requests
and schedules them via KAI Scheduler.
"""

import json
import os
import subprocess
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


# --- Kubernetes deployment ---

def _k8s_deploy(inst_id: str, fn: dict, deploy_config: dict):
    """Create a real Kubernetes Deployment with GPU requests via KAI Scheduler."""
    name = f"detoserve-{fn['name']}-{inst_id[-6:]}"
    namespace = f"tenant-{deploy_config.get('tenant_id', 'default')}"
    gpu_count = fn.get("resources", {}).get("gpu_count", 1)
    model_uri = fn.get("model_uri", "unknown")
    runtime = fn.get("runtime", "vllm")
    replicas = fn.get("scaling", {}).get("min_replicas", 1)

    # Ensure namespace exists
    subprocess.run(["kubectl", "create", "namespace", namespace],
                   capture_output=True, timeout=10)

    # Label namespace for KAI Scheduler queue
    subprocess.run(["kubectl", "label", "namespace", namespace,
                     "kai-scheduler-queue=inference-queue", "--overwrite"],
                   capture_output=True, timeout=10)

    manifest = {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {
                "app.kubernetes.io/name": name,
                "app.kubernetes.io/managed-by": "detoserve",
                "detoserve/function-id": fn.get("id", ""),
                "detoserve/instance-id": inst_id,
                "detoserve/runtime": runtime,
            },
        },
        "spec": {
            "replicas": replicas,
            "selector": {"matchLabels": {"app.kubernetes.io/name": name}},
            "template": {
                "metadata": {
                    "labels": {
                        "app.kubernetes.io/name": name,
                        "detoserve/function-id": fn.get("id", ""),
                        "detoserve/instance-id": inst_id,
                    },
                },
                "spec": {
                    "schedulerName": os.getenv("SCHEDULER_NAME", "default-scheduler"),
                    "containers": [{
                        "name": "inference",
                        "image": "python:3.11-slim",
                        "command": ["python3", "-c", f"""
import http.server, json, threading, time
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/health':
            self.send_response(200)
            self.send_header('Content-Type','application/json')
            self.end_headers()
            self.wfile.write(json.dumps({{"status":"healthy","model":"{model_uri}","runtime":"{runtime}","gpu_count":{gpu_count}}}).encode())
        elif self.path == '/v1/models':
            self.send_response(200)
            self.send_header('Content-Type','application/json')
            self.end_headers()
            self.wfile.write(json.dumps({{"data":[{{"id":"{model_uri}","object":"model"}}]}}).encode())
        else:
            self.send_response(200)
            self.send_header('Content-Type','application/json')
            self.end_headers()
            self.wfile.write(json.dumps({{"message":"DetoServe dummy inference server","model":"{model_uri}"}}).encode())
    def log_message(self, *a): pass
print(f'Dummy inference server for {model_uri} starting on :8000')
http.server.HTTPServer(('',8000),H).serve_forever()
"""],
                        "ports": [{"containerPort": 8000, "name": "http"}],
                        "resources": {
                            "requests": {"nvidia.com/gpu": str(gpu_count), "cpu": "100m", "memory": "256Mi"},
                            "limits": {"nvidia.com/gpu": str(gpu_count), "cpu": "500m", "memory": "512Mi"},
                        },
                        "readinessProbe": {
                            "httpGet": {"path": "/health", "port": 8000},
                            "initialDelaySeconds": 5,
                            "periodSeconds": 10,
                        },
                        "livenessProbe": {
                            "httpGet": {"path": "/health", "port": 8000},
                            "initialDelaySeconds": 10,
                            "periodSeconds": 30,
                        },
                    }],
                },
            },
        },
    }

    # Apply via kubectl
    proc = subprocess.run(
        ["kubectl", "apply", "-f", "-"],
        input=json.dumps(manifest),
        capture_output=True, text=True, timeout=30,
    )

    if proc.returncode != 0:
        print(f"[deploy] kubectl apply failed: {proc.stderr}")
        with _lock:
            if inst_id in _instances:
                _instances[inst_id]["status"] = "error"
                _instances[inst_id]["updated_at"] = _now()
        return

    print(f"[deploy] Created deployment {name} in {namespace} ({gpu_count} GPUs)")

    # Create a Service for the deployment
    svc = {
        "apiVersion": "v1",
        "kind": "Service",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "selector": {"app.kubernetes.io/name": name},
            "ports": [{"port": 8000, "targetPort": 8000, "name": "http"}],
        },
    }
    subprocess.run(["kubectl", "apply", "-f", "-"],
                   input=json.dumps(svc), capture_output=True, text=True, timeout=10)

    # Poll until pods are ready (max 120s)
    for _ in range(24):
        time.sleep(5)
        result = subprocess.run(
            ["kubectl", "get", "deployment", name, "-n", namespace, "-o", "json"],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode == 0:
            dep = json.loads(result.stdout)
            ready = dep.get("status", {}).get("readyReplicas", 0)
            desired = dep.get("spec", {}).get("replicas", replicas)
            with _lock:
                if inst_id in _instances:
                    _instances[inst_id]["replicas"] = ready
                    _instances[inst_id]["updated_at"] = _now()
            if ready >= desired:
                with _lock:
                    if inst_id in _instances:
                        _instances[inst_id]["status"] = "running"
                        _instances[inst_id]["endpoint"] = f"http://{name}.{namespace}.svc:8000"
                        _instances[inst_id]["updated_at"] = _now()
                print(f"[deploy] {name} is running ({ready}/{desired} replicas)")
                return

    # Timeout — mark as partial
    with _lock:
        if inst_id in _instances:
            _instances[inst_id]["status"] = "degraded"
            _instances[inst_id]["updated_at"] = _now()
    print(f"[deploy] {name} timed out waiting for readiness")


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

    threading.Thread(target=_k8s_deploy, args=(inst["id"], f, req.model_dump()), daemon=True).start()

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
        inst = _instances.pop(inst_id, None)
    if inst:
        fn_name = inst.get("function_name", "unknown")
        namespace = f"tenant-{inst.get('tenant_id', 'default')}"
        dep_name = f"detoserve-{fn_name}-{inst_id[-6:]}"
        subprocess.run(["kubectl", "delete", "deployment", dep_name, "-n", namespace, "--ignore-not-found"],
                       capture_output=True, timeout=15)
        subprocess.run(["kubectl", "delete", "service", dep_name, "-n", namespace, "--ignore-not-found"],
                       capture_output=True, timeout=15)
    return {"status": "deleted"}


if __name__ == "__main__":
    import uvicorn
    port = int(os.getenv("PORT", "8086"))
    print(f"Function Manager starting on :{port}")
    uvicorn.run(app, host="0.0.0.0", port=port)
