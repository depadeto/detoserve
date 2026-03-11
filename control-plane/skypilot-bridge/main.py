"""
SkyPilot Bridge — Python FastAPI wrapper around SkyPilot SDK.

Translates REST API calls from the Go control plane services
into SkyPilot CLI/SDK operations for cluster provisioning,
service deployment, scaling, and status queries.

Also serves as the aggregation point for cluster agent heartbeats,
providing a unified /api/clusters endpoint for the frontend.
"""

import os
import json
import subprocess
import threading
import logging
from pathlib import Path
from typing import Optional
from datetime import datetime, timezone

from fastapi import FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("skypilot-bridge")

app = FastAPI(title="SkyPilot Bridge", version="0.2.0")

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_methods=["*"],
    allow_headers=["*"],
)

SERVICES_DIR = Path(os.getenv("SERVICES_DIR", "/app/services"))

# In-memory cluster registry (agent heartbeats land here)
_clusters: dict[str, dict] = {}
_clusters_lock = threading.Lock()
STALE_THRESHOLD_SEC = 60


# --- Request Models ---

class DeployServiceRequest(BaseModel):
    name: str
    model_uri: str
    runtime: str = "vllm"
    gpu_type: str = "A100"
    gpu_count: int = 4
    tensor_parallel: int = 4
    max_model_len: int = 8192
    min_replicas: int = 2
    max_replicas: int = 20
    cloud: Optional[str] = None
    region: Optional[str] = None
    use_spot: bool = False
    extra_args: list[str] = []
    env_vars: dict[str, str] = {}


class ScaleServiceRequest(BaseModel):
    min_replicas: Optional[int] = None
    max_replicas: Optional[int] = None


class LaunchClusterRequest(BaseModel):
    name: str
    gpu_type: str = "A100"
    gpu_count: int = 8
    cloud: Optional[str] = None
    region: Optional[str] = None
    use_spot: bool = False
    idle_minutes_to_autostop: int = 60


# --- Helpers ---

def run_sky_cmd(args: list[str], timeout: int = 300) -> dict:
    cmd = ["sky"] + args
    logger.info(f"Running: {' '.join(cmd)}")
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return {
            "returncode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
        }
    except subprocess.TimeoutExpired:
        raise HTTPException(status_code=504, detail="SkyPilot command timed out")
    except FileNotFoundError:
        return {"returncode": 1, "stdout": "", "stderr": "sky CLI not found"}


def generate_service_yaml(req: DeployServiceRequest) -> str:
    if req.runtime == "vllm":
        run_cmd = (
            f"python -m vllm.entrypoints.openai.api_server "
            f"--model {req.model_uri} "
            f"--tensor-parallel-size {req.tensor_parallel} "
            f"--max-model-len {req.max_model_len} "
            f"--enable-prefix-caching "
            f"--port 8000"
        )
        if req.extra_args:
            run_cmd += " " + " ".join(req.extra_args)
        setup_cmd = "pip install vllm"
        readiness_path = "/health"
        initial_delay = 120
    elif req.runtime == "triton":
        run_cmd = "tritonserver --model-repository=/models --http-port=8000"
        setup_cmd = ""
        readiness_path = "/v2/health/ready"
        initial_delay = 60
    elif req.runtime == "dynamo":
        run_cmd = (
            f"dynamo serve --model {req.model_uri} "
            f"--disaggregated-prefill --port 8000"
        )
        if req.extra_args:
            run_cmd += " " + " ".join(req.extra_args)
        setup_cmd = "pip install nvidia-dynamo"
        readiness_path = "/health"
        initial_delay = 180
    else:
        raise HTTPException(status_code=400, detail=f"Unsupported runtime: {req.runtime}")

    resources = {"accelerators": f"{req.gpu_type}:{req.gpu_count}"}
    if req.cloud:
        resources["cloud"] = req.cloud
    if req.region:
        resources["region"] = req.region
    if req.use_spot:
        resources["use_spot"] = True

    lines = [
        "service:", "  replicas:",
        f"    min: {req.min_replicas}", f"    max: {req.max_replicas}",
        "  readiness_probe:",
        f"    path: {readiness_path}", f"    initial_delay_seconds: {initial_delay}",
        "", "resources:",
    ]
    for k, v in resources.items():
        lines.append(f"  {k}: {v}")
    lines.append("")
    if req.env_vars:
        lines.append("envs:")
        for k, v in req.env_vars.items():
            lines.append(f"  {k}: {v}")
        lines.append("")
    if setup_cmd:
        lines += ["setup: |", f"  {setup_cmd}", ""]
    lines += ["run: |", f"  {run_cmd}"]
    return "\n".join(lines)


# ========================================================================
#  CLUSTER REGISTRY — receives heartbeats from DetoServe agents
# ========================================================================

@app.post("/api/clusters/heartbeat")
async def cluster_heartbeat(request: Request):
    """Receive heartbeat from a DetoServe agent running on a cluster."""
    body = await request.json()
    cluster_id = body.get("cluster_id", "")
    if not cluster_id:
        raise HTTPException(status_code=400, detail="cluster_id required")

    body["_received_at"] = datetime.now(timezone.utc).isoformat()

    with _clusters_lock:
        _clusters[cluster_id] = body

    logger.info(
        f"Heartbeat from {cluster_id}: "
        f"{body.get('total_gpus', 0)} GPUs, "
        f"{body.get('available_gpus', 0)} available, "
        f"{len(body.get('nodes', []))} nodes"
    )
    return {"status": "ok", "cluster_id": cluster_id}


@app.get("/api/clusters")
def get_clusters():
    """Return all known clusters with their latest state."""
    now = datetime.now(timezone.utc)
    clusters = []

    with _clusters_lock:
        for cid, state in _clusters.items():
            received = state.get("_received_at", "")
            stale = False
            if received:
                try:
                    dt = datetime.fromisoformat(received)
                    stale = (now - dt).total_seconds() > STALE_THRESHOLD_SEC
                except Exception:
                    pass

            cluster = {**state}
            if stale:
                cluster["status"] = "stale"
            cluster.pop("_received_at", None)
            clusters.append(cluster)

    total_gpus = sum(c.get("total_gpus", 0) for c in clusters)
    available_gpus = sum(c.get("available_gpus", 0) for c in clusters)
    total_nodes = sum(len(c.get("nodes", [])) for c in clusters)

    return {
        "summary": {
            "cluster_count": len(clusters),
            "total_gpus": total_gpus,
            "available_gpus": available_gpus,
            "total_nodes": total_nodes,
        },
        "clusters": clusters,
    }


@app.get("/api/clusters/{cluster_id}")
def get_cluster(cluster_id: str):
    """Return a specific cluster's state."""
    with _clusters_lock:
        state = _clusters.get(cluster_id)
    if not state:
        raise HTTPException(status_code=404, detail="Cluster not found")
    result = {**state}
    result.pop("_received_at", None)
    return result


@app.delete("/api/clusters/{cluster_id}")
def remove_cluster(cluster_id: str):
    """Remove a cluster from the registry (decommission)."""
    with _clusters_lock:
        removed = _clusters.pop(cluster_id, None)
    if not removed:
        raise HTTPException(status_code=404, detail="Cluster not found")
    return {"status": "removed", "cluster_id": cluster_id}


# ========================================================================
#  SERVICE MANAGEMENT (sky serve)
# ========================================================================

@app.get("/healthz")
def health():
    return {"status": "ok"}


@app.post("/api/services")
def deploy_service(req: DeployServiceRequest):
    yaml_content = generate_service_yaml(req)
    yaml_path = SERVICES_DIR / f"{req.name}.yaml"
    yaml_path.parent.mkdir(parents=True, exist_ok=True)
    yaml_path.write_text(yaml_content)
    logger.info(f"Generated service YAML for {req.name}:\n{yaml_content}")
    result = run_sky_cmd(["serve", "up", str(yaml_path), "-n", req.name, "-y"], timeout=600)
    if result["returncode"] != 0:
        return {"status": "error", "service_name": req.name, "error": result["stderr"], "yaml": yaml_content}
    return {"status": "deploying", "service_name": req.name, "yaml": yaml_content, "output": result["stdout"]}


@app.get("/api/services")
def list_services():
    result = run_sky_cmd(["serve", "status"])
    return {"output": result["stdout"], "error": result["stderr"] if result["returncode"] != 0 else None}


@app.get("/api/services/{name}")
def get_service(name: str):
    result = run_sky_cmd(["serve", "status", name])
    return {"service_name": name, "output": result["stdout"], "error": result["stderr"] if result["returncode"] != 0 else None}


@app.patch("/api/services/{name}/scale")
def scale_service(name: str, req: ScaleServiceRequest):
    args = ["serve", "update", name]
    if req.min_replicas is not None:
        args.extend(["--min-replicas", str(req.min_replicas)])
    if req.max_replicas is not None:
        args.extend(["--max-replicas", str(req.max_replicas)])
    result = run_sky_cmd(args)
    return {"service_name": name, "status": "scaling" if result["returncode"] == 0 else "error", "output": result["stdout"]}


@app.delete("/api/services/{name}")
def delete_service(name: str):
    result = run_sky_cmd(["serve", "down", name, "-y"])
    return {"service_name": name, "status": "deleting" if result["returncode"] == 0 else "error", "output": result["stdout"]}


# ========================================================================
#  CLUSTER MANAGEMENT (sky launch / status)
# ========================================================================

@app.post("/api/clusters/provision")
def launch_cluster(req: LaunchClusterRequest):
    args = ["launch", "-c", req.name, "--gpus", f"{req.gpu_type}:{req.gpu_count}", "-y",
            "--idle-minutes-to-autostop", str(req.idle_minutes_to_autostop)]
    if req.cloud:
        args.extend(["--cloud", req.cloud])
    if req.region:
        args.extend(["--region", req.region])
    if req.use_spot:
        args.append("--use-spot")
    result = run_sky_cmd(args, timeout=600)
    return {"cluster_name": req.name, "status": "launching" if result["returncode"] == 0 else "error",
            "output": result["stdout"], "error": result["stderr"] if result["returncode"] != 0 else None}


@app.delete("/api/clusters/provision/{name}")
def teardown_cluster(name: str):
    result = run_sky_cmd(["down", name, "-y"])
    return {"cluster_name": name, "status": "terminating" if result["returncode"] == 0 else "error", "output": result["stdout"]}


@app.get("/api/resources")
def check_resources():
    result = run_sky_cmd(["check"])
    return {"output": result["stdout"]}


@app.get("/api/gpus")
def list_gpus(cloud: Optional[str] = None):
    args = ["show-gpus"]
    if cloud:
        args.extend(["--cloud", cloud])
    result = run_sky_cmd(args)
    return {"output": result["stdout"]}


if __name__ == "__main__":
    import uvicorn
    port = int(os.getenv("PORT", "8085"))
    uvicorn.run(app, host="0.0.0.0", port=port)
