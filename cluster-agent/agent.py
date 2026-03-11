#!/usr/bin/env python3
"""
DetoServe Cluster Agent

Deployed on each onboarded cluster (via Helm or standalone).
Discovers GPUs from Kubernetes node metadata and sends heartbeats
to the DetoServe control plane.

Environment variables:
  CONTROL_PLANE_URL  - Control plane endpoint (default: http://localhost:8085)
  CLUSTER_ID         - Unique cluster identifier (auto-detected if empty)
  CLUSTER_NAME       - Human-friendly name (defaults to CLUSTER_ID)
  REPORT_INTERVAL    - Heartbeat interval in seconds (default: 10)
  API_TOKEN          - Bearer token for control plane auth (optional)
  PORT               - Health/status server port (default: 9090)
"""

import json
import os
import subprocess
import threading
import time
from http.server import HTTPServer, BaseHTTPRequestHandler

CONTROL_PLANE_URL = os.getenv("CONTROL_PLANE_URL", "http://localhost:8085")
CLUSTER_ID = os.getenv("CLUSTER_ID", "")
CLUSTER_NAME = os.getenv("CLUSTER_NAME", "")
REPORT_INTERVAL = int(os.getenv("REPORT_INTERVAL", "10"))
API_TOKEN = os.getenv("API_TOKEN", "")
PORT = int(os.getenv("PORT", "9090"))

state = {}


def detect_cluster_id():
    try:
        r = subprocess.run(["kubectl", "config", "current-context"],
                           capture_output=True, text=True, timeout=5)
        return r.stdout.strip() if r.returncode == 0 else f"unknown-{int(time.time())}"
    except Exception:
        return f"unknown-{int(time.time())}"


def detect_provider(ctx):
    if ctx.startswith("k3d-"):
        return "k3d (local)"
    if "eks" in ctx:
        return "AWS EKS"
    if "gke" in ctx:
        return "GCP GKE"
    if "aks" in ctx:
        return "Azure AKS"
    return "Kubernetes"


def _get_gpu_usage_per_node():
    """Query all non-terminal pods and sum GPU requests per node."""
    usage = {}
    try:
        r = subprocess.run(
            ["kubectl", "get", "pods", "--all-namespaces", "--field-selector=status.phase!=Succeeded,status.phase!=Failed", "-o", "json"],
            capture_output=True, text=True, timeout=15,
        )
        if r.returncode != 0:
            return usage
        pods = json.loads(r.stdout)
        for pod in pods.get("items", []):
            node_name = pod.get("spec", {}).get("nodeName", "")
            if not node_name:
                continue
            for container in pod.get("spec", {}).get("containers", []):
                req = container.get("resources", {}).get("requests", {})
                gpu_req = req.get("nvidia.com/gpu", "0")
                gpu_val = int(gpu_req) if str(gpu_req).isdigit() else 0
                if gpu_val > 0:
                    usage[node_name] = usage.get(node_name, 0) + gpu_val
    except Exception as e:
        print(f"[discover] pod GPU scan error: {e}")
    return usage


def discover():
    global state
    cluster_id = CLUSTER_ID or detect_cluster_id()
    cluster_name = CLUSTER_NAME or cluster_id

    try:
        r = subprocess.run(["kubectl", "get", "nodes", "-o", "json"],
                           capture_output=True, text=True, timeout=15)
        if r.returncode != 0:
            print(f"[discover] kubectl failed: {r.stderr}")
            return
        data = json.loads(r.stdout)
    except Exception as e:
        print(f"[discover] error: {e}")
        return

    gpu_used_per_node = _get_gpu_usage_per_node()

    nodes = []
    total_gpus = 0
    avail_gpus = 0
    gpu_map = {}

    for item in data.get("items", []):
        meta = item.get("metadata", {})
        labels = meta.get("labels", {})
        st = item.get("status", {})
        cap = st.get("capacity", {})
        alloc = st.get("allocatable", {})
        conditions = st.get("conditions", [])
        ni = st.get("nodeInfo", {})

        node_name = meta.get("name", "")
        gpu_cap = int(cap.get("nvidia.com/gpu", "0"))
        gpu_used = gpu_used_per_node.get(node_name, 0)
        gpu_free = max(0, gpu_cap - gpu_used)

        ready = any(c["type"] == "Ready" and c["status"] == "True" for c in conditions)

        role = "worker"
        for k in labels:
            if k.startswith("node-role.kubernetes.io/"):
                role = k.split("/")[1]

        mem_str = cap.get("memory", "0")
        mem_num = int("".join(c for c in mem_str if c.isdigit()) or "0")
        mem_gb = f"{mem_num / 1024 / 1024:.1f}"

        gpu_type = labels.get("nvidia.com/gpu.machine", "")
        gpu_family = labels.get("nvidia.com/gpu.family", "")

        node = {
            "name": node_name,
            "status": "Ready" if ready else "NotReady",
            "role": role,
            "cpu": cap.get("cpu", "0"),
            "memory_gb": mem_gb,
            "gpu_type": gpu_type,
            "gpu_count": gpu_cap,
            "gpu_used": gpu_used,
            "gpu_available": gpu_free,
            "gpu_family": gpu_family,
            "k8s_version": ni.get("kubeletVersion", ""),
            "container_runtime": ni.get("containerRuntimeVersion", ""),
        }
        nodes.append(node)
        total_gpus += gpu_cap
        avail_gpus += gpu_free

        if gpu_type:
            if gpu_type not in gpu_map:
                gpu_map[gpu_type] = {"name": gpu_type, "family": gpu_family, "count": 0, "available": 0}
            gpu_map[gpu_type]["count"] += gpu_cap
            gpu_map[gpu_type]["available"] += gpu_free

    k8s_version = nodes[0]["k8s_version"] if nodes else ""

    state = {
        "cluster_id": cluster_id,
        "cluster_name": cluster_name,
        "status": "healthy",
        "provider": detect_provider(cluster_id),
        "k8s_version": k8s_version,
        "total_gpus": total_gpus,
        "available_gpus": avail_gpus,
        "gpu_types": list(gpu_map.values()),
        "nodes": nodes,
    }
    print(f"[discover] cluster={cluster_id} gpus={avail_gpus}/{total_gpus} nodes={len(nodes)}")


def send_heartbeat():
    if not state:
        return

    import urllib.request
    url = CONTROL_PLANE_URL.rstrip("/") + "/api/clusters/heartbeat"
    data = json.dumps(state).encode()

    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if API_TOKEN:
        req.add_header("Authorization", f"Bearer {API_TOKEN}")

    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            body = resp.read().decode()
            print(f"[heartbeat] sent to {url} -> {resp.status} {body}")
    except Exception as e:
        print(f"[heartbeat] failed: {e}")


def heartbeat_loop():
    while True:
        discover()
        send_heartbeat()
        time.sleep(REPORT_INTERVAL)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        elif self.path == "/status":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(state, indent=2).encode())
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt, *args):
        pass


def main():
    print(f"DetoServe Agent starting (control-plane={CONTROL_PLANE_URL}, interval={REPORT_INTERVAL}s, port={PORT})")

    t = threading.Thread(target=heartbeat_loop, daemon=True)
    t.start()

    server = HTTPServer(("", PORT), Handler)
    print(f"Health/status server on :{PORT}")
    server.serve_forever()


if __name__ == "__main__":
    main()
