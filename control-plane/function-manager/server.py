"""
Function Manager — deploys inference workloads via SkyPilot

A "Function" is a reusable template: define once, deploy many times.
An "Instance" is a running deployment of that function on a cluster.

Deployment flow:
  1. User creates a Function (model + resource spec)
  2. User deploys an Instance — we call sky.launch() which:
     - Picks the best cluster/node based on GPU availability
     - Creates a pod with the right GPU requests
     - Runs the inference server inside
  3. SkyPilot handles multi-cluster selection, GPU matching, lifecycle
"""

import asyncio
import json
import os
import subprocess
import time
import threading
import traceback
from datetime import datetime, timezone
from typing import Optional

from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel, Field

try:
    import sky
    HAS_SKYPILOT = True
except ImportError:
    HAS_SKYPILOT = False

app = FastAPI(title="Function Manager", version="0.2.0")

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
    sky_cluster_name: str = ""
    deployed_via: str = ""
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

DEV_MODE = os.getenv("DEV_MODE", "true").lower() == "true"


def _now():
    return datetime.now(timezone.utc).isoformat()


# --- GPU type mapping ---

GPU_TYPE_MAP = {
    "H100": "H100-80GB",
    "A100": "A100-80GB",
    "A100-80GB": "A100-80GB",
    "H100-80GB": "H100-80GB",
    "L40S": "L40S",
    "V100": "V100",
}


def _resolve_gpu_name(gpu_type: str) -> str:
    """Map user-facing GPU names to SkyPilot accelerator names."""
    return GPU_TYPE_MAP.get(gpu_type, gpu_type)


# --- SkyPilot deployment ---

def _build_run_script(model_uri: str, runtime: str, gpu_count: int) -> str:
    """Build the inference server run command for the SkyPilot task."""
    return f"""
python3 -c "
import http.server, json, time
class H(http.server.BaseHTTPRequestHandler):
    def _respond(self, data, code=200):
        self.send_response(code)
        self.send_header('Content-Type','application/json')
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())
    def do_GET(self):
        if self.path == '/health':
            self._respond({{'status':'healthy','model':'{model_uri}','runtime':'{runtime}','gpu_count':{gpu_count}}})
        elif self.path == '/v1/models':
            self._respond({{'object':'list','data':[{{'id':'{model_uri}','object':'model','owned_by':'detoserve'}}]}})
        else:
            self._respond({{'message':'DetoServe inference','model':'{model_uri}'}})
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = json.loads(self.rfile.read(length)) if length else {{}}
        if '/v1/chat/completions' in self.path:
            msgs = body.get('messages', [])
            last = msgs[-1]['content'] if msgs else 'hello'
            self._respond({{
                'id': 'chatcmpl-detoserve',
                'object': 'chat.completion',
                'created': int(time.time()),
                'model': '{model_uri}',
                'choices': [{{
                    'index': 0,
                    'message': {{'role': 'assistant', 'content': f'[DetoServe {runtime} on {gpu_count}x GPU] Echo: {{last}}'}},
                    'finish_reason': 'stop'
                }}],
                'usage': {{'prompt_tokens': len(last.split()), 'completion_tokens': 10, 'total_tokens': len(last.split()) + 10}}
            }})
        elif '/v1/completions' in self.path:
            prompt = body.get('prompt', '')
            self._respond({{
                'id': 'cmpl-detoserve',
                'object': 'text_completion',
                'created': int(time.time()),
                'model': '{model_uri}',
                'choices': [{{'text': f'[DetoServe] {{prompt}}...continued', 'index': 0, 'finish_reason': 'stop'}}]
            }})
        else:
            self._respond({{'message': 'use /v1/chat/completions or /v1/completions'}})
    def log_message(self, *a): pass
print('Inference server for {model_uri} on :8080')
http.server.HTTPServer(('',8080),H).serve_forever()
"
"""


_port_counter = 9100

def _get_skypilot_endpoint(sky_cluster: str, max_retries: int = 12) -> str:
    """Discover the endpoint and set up a port-forward for local access.

    SkyPilot creates a LoadBalancer service after the pod is running,
    so we retry with back-off to allow the service to appear.
    """
    global _port_counter

    for attempt in range(max_retries):
        try:
            result = subprocess.run(
                ["kubectl", "get", "svc", "-n", "default", "-o", "json"],
                capture_output=True, text=True, timeout=10,
            )
            if result.returncode != 0:
                time.sleep(5)
                continue

            svcs = json.loads(result.stdout).get("items", [])
            lb_svc_name = None
            for svc in svcs:
                svc_name = svc["metadata"]["name"]
                if "skypilot-lb" not in svc_name:
                    continue
                cluster_slug = sky_cluster.replace("-", "")
                if cluster_slug not in svc_name.replace("-", ""):
                    continue
                for port_spec in svc.get("spec", {}).get("ports", []):
                    if port_spec.get("port") == 8080:
                        lb_svc_name = svc_name
                        break
                if lb_svc_name:
                    break

            if not lb_svc_name:
                print(f"[endpoint] {sky_cluster}: LB service not found yet (attempt {attempt+1}/{max_retries})")
                time.sleep(10)
                continue

            local_port = _port_counter
            _port_counter += 1

            subprocess.Popen(
                ["kubectl", "port-forward", f"svc/{lb_svc_name}", f"{local_port}:8080",
                 "-n", "default"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            time.sleep(2)

            endpoint = f"http://localhost:{local_port}"
            print(f"[endpoint] {sky_cluster} -> {lb_svc_name} -> {endpoint}")
            return endpoint

        except Exception as e:
            print(f"[endpoint] Error discovering endpoint (attempt {attempt+1}): {e}")
            time.sleep(5)

    print(f"[endpoint] {sky_cluster}: gave up after {max_retries} retries")
    return ""


def _skypilot_deploy(inst_id: str, fn: dict, deploy_config: dict):
    """Deploy via SkyPilot — picks the best cluster and node automatically."""
    # SkyPilot uses asyncio internally; create a loop for this thread
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    gpu_count = fn.get("resources", {}).get("gpu_count", 1)
    gpu_type = _resolve_gpu_name(fn.get("resources", {}).get("gpu_type", "A100"))
    model_uri = fn.get("model_uri", "unknown")
    runtime = fn.get("runtime", "vllm")
    fn_name = fn.get("name", "func").replace(" ", "-").lower()

    sky_cluster = f"detoserve-{fn_name}-{inst_id[-6:]}"

    with _lock:
        if inst_id in _instances:
            _instances[inst_id]["sky_cluster_name"] = sky_cluster
            _instances[inst_id]["deployed_via"] = "skypilot"

    try:
        task = sky.Task(
            name=f"detoserve-{fn_name}",
            run=_build_run_script(model_uri, runtime, gpu_count),
        )

        resource_kwargs = {
            "cloud": sky.Kubernetes(),
            "accelerators": f"{gpu_type}:{gpu_count}",
            "ports": 8080,
        }
        if DEV_MODE:
            resource_kwargs["cpus"] = 1
            resource_kwargs["memory"] = 2

        task.set_resources(sky.Resources(**resource_kwargs))

        print(f"[deploy/skypilot] Launching {sky_cluster} with {gpu_count}x {gpu_type}")

        request_id = sky.launch(
            task,
            cluster_name=sky_cluster,
            retry_until_up=False,
        )

        result = sky.stream_and_get(request_id)
        print(f"[deploy/skypilot] {sky_cluster} launch result: {result}")

        endpoint = _get_skypilot_endpoint(sky_cluster)

        with _lock:
            if inst_id in _instances:
                _instances[inst_id]["status"] = "running"
                _instances[inst_id]["cluster"] = sky_cluster
                _instances[inst_id]["endpoint"] = endpoint
                _instances[inst_id]["updated_at"] = _now()
        print(f"[deploy/skypilot] {sky_cluster} is running — endpoint: {endpoint}")

    except Exception as e:
        print(f"[deploy/skypilot] FAILED: {e}")
        traceback.print_exc()
        with _lock:
            if inst_id in _instances:
                _instances[inst_id]["status"] = "error"
                _instances[inst_id]["updated_at"] = _now()


def _skypilot_delete(inst: dict):
    """Tear down a SkyPilot cluster."""
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    sky_cluster = inst.get("sky_cluster_name", "")
    if not sky_cluster:
        return
    try:
        request_id = sky.down(sky_cluster)
        sky.stream_and_get(request_id)
        print(f"[delete/skypilot] {sky_cluster} terminated")
    except Exception as e:
        print(f"[delete/skypilot] {sky_cluster} error: {e}")


# --- Fallback: direct kubectl deployment (legacy) ---

def _kubectl_deploy(inst_id: str, fn: dict, deploy_config: dict):
    """Fallback: create Kubernetes Deployment directly via kubectl."""
    name = f"detoserve-{fn['name']}-{inst_id[-6:]}"
    namespace = f"tenant-{deploy_config.get('tenant_id', 'default')}"
    gpu_count = fn.get("resources", {}).get("gpu_count", 1)
    model_uri = fn.get("model_uri", "unknown")
    runtime = fn.get("runtime", "vllm")
    replicas = fn.get("scaling", {}).get("min_replicas", 1)

    with _lock:
        if inst_id in _instances:
            _instances[inst_id]["deployed_via"] = "kubectl"

    subprocess.run(["kubectl", "create", "namespace", namespace],
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
                "detoserve/instance-id": inst_id,
            },
        },
        "spec": {
            "replicas": replicas,
            "selector": {"matchLabels": {"app.kubernetes.io/name": name}},
            "template": {
                "metadata": {
                    "labels": {
                        "app.kubernetes.io/name": name,
                        "detoserve/instance-id": inst_id,
                        "kai.scheduler/queue": "inference-queue",
                    },
                },
                "spec": {
                    "schedulerName": "kai-scheduler",
                    "containers": [{
                        "name": "inference",
                        "image": "python:3.11-slim",
                        "command": ["python3", "-c", _build_run_script(model_uri, runtime, gpu_count).strip().split('"', 1)[1].rsplit('"', 1)[0] if False else f"""
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type','application/json')
        self.end_headers()
        if self.path == '/health':
            self.wfile.write(json.dumps({{"status":"healthy","model":"{model_uri}"}}).encode())
        else:
            self.wfile.write(json.dumps({{"message":"DetoServe inference","model":"{model_uri}"}}).encode())
    def log_message(self, *a): pass
print('Inference server on :8000')
http.server.HTTPServer(('',8000),H).serve_forever()
"""],
                        "ports": [{"containerPort": 8000}],
                        "resources": {
                            "requests": {"nvidia.com/gpu": str(gpu_count)},
                            "limits": {"nvidia.com/gpu": str(gpu_count)},
                        },
                    }],
                },
            },
        },
    }

    proc = subprocess.run(
        ["kubectl", "apply", "-f", "-"],
        input=json.dumps(manifest),
        capture_output=True, text=True, timeout=30,
    )
    if proc.returncode != 0:
        print(f"[deploy/kubectl] failed: {proc.stderr}")
        with _lock:
            if inst_id in _instances:
                _instances[inst_id]["status"] = "error"
                _instances[inst_id]["updated_at"] = _now()
        return

    print(f"[deploy/kubectl] Created {name} in {namespace}")

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
                print(f"[deploy/kubectl] {name} is running")
                return

    with _lock:
        if inst_id in _instances:
            _instances[inst_id]["status"] = "degraded"
            _instances[inst_id]["updated_at"] = _now()


def _deploy(inst_id: str, fn: dict, deploy_config: dict):
    """Route deployment through SkyPilot or fall back to kubectl."""
    if HAS_SKYPILOT:
        try:
            _skypilot_deploy(inst_id, fn, deploy_config)
            return
        except Exception as e:
            print(f"[deploy] SkyPilot failed, falling back to kubectl: {e}")
    _kubectl_deploy(inst_id, fn, deploy_config)


def _delete_instance_resources(inst: dict):
    """Clean up deployment resources."""
    deployed_via = inst.get("deployed_via", "")
    if deployed_via == "skypilot" and HAS_SKYPILOT:
        _skypilot_delete(inst)
    else:
        fn_name = inst.get("function_name", "unknown")
        namespace = f"tenant-{inst.get('tenant_id', 'default')}"
        inst_id = inst.get("id", "")
        dep_name = f"detoserve-{fn_name}-{inst_id[-6:]}"
        subprocess.run(["kubectl", "delete", "deployment", dep_name, "-n", namespace, "--ignore-not-found"],
                       capture_output=True, timeout=15)
        subprocess.run(["kubectl", "delete", "service", dep_name, "-n", namespace, "--ignore-not-found"],
                       capture_output=True, timeout=15)


# --- Endpoints ---

@app.get("/healthz")
def health():
    return {"status": "ok", "skypilot": HAS_SKYPILOT, "dev_mode": DEV_MODE}


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
        "sky_cluster_name": "",
        "deployed_via": "",
        "created_at": _now(),
        "updated_at": _now(),
    }

    with _lock:
        _instances[inst["id"]] = inst

    threading.Thread(target=_deploy, args=(inst["id"], f, req.model_dump()), daemon=True).start()

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


@app.post("/api/instances/seed")
def seed_instance(body: dict):
    """Register a pre-existing running instance (for recovering from FM restart)."""
    required = ["function_id", "function_name", "tenant_id", "endpoint"]
    for key in required:
        if key not in body:
            raise HTTPException(400, f"missing required field: {key}")
    inst_id = f"inst-{int(time.time() * 1000)}"
    inst = {
        "id": inst_id,
        "function_id": body["function_id"],
        "function_name": body["function_name"],
        "tenant_id": body.get("tenant_id", "default"),
        "status": "running",
        "endpoint": body["endpoint"],
        "cluster": body.get("cluster", ""),
        "sky_cluster_name": body.get("sky_cluster_name", ""),
        "deployed_via": body.get("deployed_via", "skypilot"),
        "replicas": body.get("replicas", 1),
        "created_at": _now(),
        "updated_at": _now(),
    }
    with _lock:
        _instances[inst_id] = inst
    return inst


@app.patch("/api/instances/{inst_id}")
def patch_instance(inst_id: str, body: dict):
    """Update instance fields (e.g. endpoint) for manual fixes."""
    with _lock:
        inst = _instances.get(inst_id)
        if not inst:
            raise HTTPException(404, "instance not found")
        for key in ("endpoint", "status", "cluster"):
            if key in body:
                inst[key] = body[key]
        inst["updated_at"] = _now()
    return inst


@app.delete("/api/instances/{inst_id}")
def delete_instance(inst_id: str):
    with _lock:
        inst = _instances.pop(inst_id, None)
    if inst:
        threading.Thread(target=_delete_instance_resources, args=(inst,), daemon=True).start()
    return {"status": "deleted"}


if __name__ == "__main__":
    import uvicorn
    port = int(os.getenv("PORT", "8086"))
    print(f"Function Manager starting on :{port}")
    print(f"  SkyPilot: {'enabled' if HAS_SKYPILOT else 'disabled (install skypilot)'}")
    print(f"  Dev mode: {DEV_MODE} (relaxed CPU/memory for fake GPUs)")
    uvicorn.run(app, host="0.0.0.0", port=port)
