"""
SkyPilot Bridge — Python FastAPI wrapper around SkyPilot SDK.

Translates REST API calls from the Go control plane services
into SkyPilot CLI/SDK operations for cluster provisioning,
service deployment, scaling, and status queries.
"""

import os
import json
import subprocess
import tempfile
import logging
from pathlib import Path
from typing import Optional

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("skypilot-bridge")

app = FastAPI(title="SkyPilot Bridge", version="0.1.0")

SERVICES_DIR = Path(os.getenv("SERVICES_DIR", "/app/services"))


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
    """Execute a sky CLI command and return result."""
    cmd = ["sky"] + args
    logger.info(f"Running: {' '.join(cmd)}")
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        return {
            "returncode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
        }
    except subprocess.TimeoutExpired:
        raise HTTPException(status_code=504, detail="SkyPilot command timed out")


def generate_service_yaml(req: DeployServiceRequest) -> str:
    """Generate a SkyPilot service YAML from a deployment request."""

    # Build vLLM command
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
        run_cmd = (
            "tritonserver "
            "--model-repository=/models "
            "--http-port=8000"
        )
        setup_cmd = ""
        readiness_path = "/v2/health/ready"
        initial_delay = 60

    elif req.runtime == "dynamo":
        run_cmd = (
            f"dynamo serve "
            f"--model {req.model_uri} "
            f"--disaggregated-prefill "
            f"--port 8000"
        )
        if req.extra_args:
            run_cmd += " " + " ".join(req.extra_args)
        setup_cmd = "pip install nvidia-dynamo"
        readiness_path = "/health"
        initial_delay = 180

    else:
        raise HTTPException(
            status_code=400,
            detail=f"Unsupported runtime: {req.runtime}",
        )

    # Build resources section
    resources = {
        "accelerators": f"{req.gpu_type}:{req.gpu_count}",
    }
    if req.cloud:
        resources["cloud"] = req.cloud
    if req.region:
        resources["region"] = req.region
    if req.use_spot:
        resources["use_spot"] = True

    # Build full YAML
    yaml_parts = []
    yaml_parts.append("service:")
    yaml_parts.append("  replicas:")
    yaml_parts.append(f"    min: {req.min_replicas}")
    yaml_parts.append(f"    max: {req.max_replicas}")
    yaml_parts.append("  readiness_probe:")
    yaml_parts.append(f"    path: {readiness_path}")
    yaml_parts.append(f"    initial_delay_seconds: {initial_delay}")
    yaml_parts.append("")
    yaml_parts.append("resources:")
    for k, v in resources.items():
        yaml_parts.append(f"  {k}: {v}")
    yaml_parts.append("")

    if req.env_vars:
        yaml_parts.append("envs:")
        for k, v in req.env_vars.items():
            yaml_parts.append(f"  {k}: {v}")
        yaml_parts.append("")

    if setup_cmd:
        yaml_parts.append("setup: |")
        yaml_parts.append(f"  {setup_cmd}")
        yaml_parts.append("")

    yaml_parts.append("run: |")
    yaml_parts.append(f"  {run_cmd}")

    return "\n".join(yaml_parts)


# --- API Endpoints ---

# Health
@app.get("/healthz")
def health():
    return {"status": "ok"}


# --- Service Management (sky serve) ---

@app.post("/api/services")
def deploy_service(req: DeployServiceRequest):
    """Deploy a model as a SkyPilot service."""
    yaml_content = generate_service_yaml(req)

    # Write YAML to temp file
    yaml_path = SERVICES_DIR / f"{req.name}.yaml"
    yaml_path.parent.mkdir(parents=True, exist_ok=True)
    yaml_path.write_text(yaml_content)

    logger.info(f"Generated service YAML for {req.name}:\n{yaml_content}")

    # Deploy via sky serve
    result = run_sky_cmd(
        ["serve", "up", str(yaml_path), "-n", req.name, "-y"],
        timeout=600,
    )

    if result["returncode"] != 0:
        return {
            "status": "error",
            "service_name": req.name,
            "error": result["stderr"],
            "yaml": yaml_content,
        }

    return {
        "status": "deploying",
        "service_name": req.name,
        "yaml": yaml_content,
        "output": result["stdout"],
    }


@app.get("/api/services")
def list_services():
    """List all SkyPilot services."""
    result = run_sky_cmd(["serve", "status"])
    return {
        "output": result["stdout"],
        "error": result["stderr"] if result["returncode"] != 0 else None,
    }


@app.get("/api/services/{name}")
def get_service(name: str):
    """Get status of a specific service."""
    result = run_sky_cmd(["serve", "status", name])
    return {
        "service_name": name,
        "output": result["stdout"],
        "error": result["stderr"] if result["returncode"] != 0 else None,
    }


@app.patch("/api/services/{name}/scale")
def scale_service(name: str, req: ScaleServiceRequest):
    """Scale a SkyPilot service replicas."""
    args = ["serve", "update", name]
    if req.min_replicas is not None:
        args.extend(["--min-replicas", str(req.min_replicas)])
    if req.max_replicas is not None:
        args.extend(["--max-replicas", str(req.max_replicas)])

    result = run_sky_cmd(args)
    return {
        "service_name": name,
        "status": "scaling" if result["returncode"] == 0 else "error",
        "output": result["stdout"],
    }


@app.delete("/api/services/{name}")
def delete_service(name: str):
    """Tear down a SkyPilot service."""
    result = run_sky_cmd(["serve", "down", name, "-y"])
    return {
        "service_name": name,
        "status": "deleting" if result["returncode"] == 0 else "error",
        "output": result["stdout"],
    }


# --- Cluster Management (sky launch / status) ---

@app.post("/api/clusters")
def launch_cluster(req: LaunchClusterRequest):
    """Provision a new GPU cluster via SkyPilot."""
    args = [
        "launch",
        "-c", req.name,
        "--gpus", f"{req.gpu_type}:{req.gpu_count}",
        "-y",
        "--idle-minutes-to-autostop", str(req.idle_minutes_to_autostop),
    ]
    if req.cloud:
        args.extend(["--cloud", req.cloud])
    if req.region:
        args.extend(["--region", req.region])
    if req.use_spot:
        args.append("--use-spot")

    result = run_sky_cmd(args, timeout=600)
    return {
        "cluster_name": req.name,
        "status": "launching" if result["returncode"] == 0 else "error",
        "output": result["stdout"],
        "error": result["stderr"] if result["returncode"] != 0 else None,
    }


@app.get("/api/clusters")
def list_clusters():
    """List all SkyPilot-managed clusters."""
    result = run_sky_cmd(["status"])
    return {"output": result["stdout"]}


@app.get("/api/clusters/{name}")
def get_cluster(name: str):
    """Get status of a specific cluster."""
    result = run_sky_cmd(["status", name])
    return {
        "cluster_name": name,
        "output": result["stdout"],
    }


@app.delete("/api/clusters/{name}")
def teardown_cluster(name: str):
    """Tear down a SkyPilot cluster."""
    result = run_sky_cmd(["down", name, "-y"])
    return {
        "cluster_name": name,
        "status": "terminating" if result["returncode"] == 0 else "error",
        "output": result["stdout"],
    }


# --- Resource Check ---

@app.get("/api/resources")
def check_resources():
    """Show available GPU resources across clouds."""
    result = run_sky_cmd(["check"])
    return {"output": result["stdout"]}


@app.get("/api/gpus")
def list_gpus(cloud: Optional[str] = None):
    """List available GPU types and pricing."""
    args = ["show-gpus"]
    if cloud:
        args.extend(["--cloud", cloud])
    result = run_sky_cmd(args)
    return {"output": result["stdout"]}


if __name__ == "__main__":
    import uvicorn
    port = int(os.getenv("PORT", "8085"))
    uvicorn.run(app, host="0.0.0.0", port=port)
