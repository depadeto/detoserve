"""
Dev cluster API — simulates the SkyPilot Bridge /api/clusters endpoint
by querying the local Kubernetes cluster directly.

In production, the DetoServe agent on each cluster sends heartbeats
to the SkyPilot Bridge, which aggregates them. This dev server
does it locally so you can test the frontend without deploying agents.
"""
import json
import subprocess
from http.server import HTTPServer, BaseHTTPRequestHandler


def _get_gpu_usage_per_node():
    """Sum GPU requests from all running pods, grouped by node."""
    usage = {}
    try:
        r = subprocess.run(
            ["kubectl", "get", "pods", "--all-namespaces",
             "--field-selector=status.phase!=Succeeded,status.phase!=Failed", "-o", "json"],
            capture_output=True, text=True, timeout=15)
        if r.returncode != 0:
            return usage
        for pod in json.loads(r.stdout).get("items", []):
            node_name = pod.get("spec", {}).get("nodeName", "")
            if not node_name:
                continue
            for c in pod.get("spec", {}).get("containers", []):
                gpu_req = c.get("resources", {}).get("requests", {}).get("nvidia.com/gpu", "0")
                gpu_val = int(gpu_req) if str(gpu_req).isdigit() else 0
                if gpu_val > 0:
                    usage[node_name] = usage.get(node_name, 0) + gpu_val
    except Exception:
        pass
    return usage


def get_cluster_state():
    result = subprocess.run(["kubectl", "get", "nodes", "-o", "json"], capture_output=True, text=True)
    if result.returncode != 0:
        return None

    data = json.loads(result.stdout)
    ctx = subprocess.run(["kubectl", "config", "current-context"], capture_output=True, text=True)
    cluster_id = ctx.stdout.strip() if ctx.returncode == 0 else "unknown"

    gpu_used_per_node = _get_gpu_usage_per_node()

    nodes = []
    total_gpus = 0
    available_gpus = 0
    gpu_map = {}

    for item in data.get("items", []):
        meta = item["metadata"]
        labels = meta.get("labels", {})
        status = item.get("status", {})
        cap = status.get("capacity", {})
        conditions = status.get("conditions", [])
        node_info = status.get("nodeInfo", {})

        node_name = meta["name"]
        gpu_cap = int(cap.get("nvidia.com/gpu", "0"))
        gpu_used = gpu_used_per_node.get(node_name, 0)
        gpu_free = max(0, gpu_cap - gpu_used)

        ready = any(c["type"] == "Ready" and c["status"] == "True" for c in conditions)
        role = "worker"
        for k in labels:
            if k.startswith("node-role.kubernetes.io/"):
                role = k.split("/")[1]

        mem_ki = int("".join(c for c in cap.get("memory", "0") if c.isdigit()) or "0")
        mem_gb = f"{mem_ki / 1024 / 1024:.1f}"

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
            "k8s_version": node_info.get("kubeletVersion", ""),
            "container_runtime": node_info.get("containerRuntimeVersion", ""),
        }
        nodes.append(node)
        total_gpus += gpu_cap
        available_gpus += gpu_free

        if gpu_type:
            if gpu_type not in gpu_map:
                gpu_map[gpu_type] = {"name": gpu_type, "family": gpu_family, "count": 0, "available": 0}
            gpu_map[gpu_type]["count"] += gpu_cap
            gpu_map[gpu_type]["available"] += gpu_free

    provider = "Kubernetes"
    if cluster_id.startswith("k3d-"):
        provider = "k3d (local)"
    elif "eks" in cluster_id:
        provider = "AWS EKS"
    elif "gke" in cluster_id:
        provider = "GCP GKE"

    cluster = {
        "cluster_id": cluster_id,
        "cluster_name": cluster_id,
        "status": "healthy",
        "provider": provider,
        "k8s_version": nodes[0]["k8s_version"] if nodes else "",
        "total_gpus": total_gpus,
        "available_gpus": available_gpus,
        "gpu_types": list(gpu_map.values()),
        "nodes": nodes,
    }
    return cluster


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/api/clusters":
            cluster = get_cluster_state()
            if not cluster:
                resp = {"summary": {"cluster_count": 0, "total_gpus": 0, "available_gpus": 0, "total_nodes": 0}, "clusters": []}
            else:
                resp = {
                    "summary": {
                        "cluster_count": 1,
                        "total_gpus": cluster["total_gpus"],
                        "available_gpus": cluster["available_gpus"],
                        "total_nodes": len(cluster["nodes"]),
                    },
                    "clusters": [cluster],
                }
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(json.dumps(resp, indent=2).encode())
        elif self.path.startswith("/api/clusters/heartbeat"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        if self.path == "/api/clusters/heartbeat":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
        else:
            self.send_response(404)
            self.end_headers()

    def do_OPTIONS(self):
        self.send_response(200)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "*")
        self.end_headers()

    def log_message(self, fmt, *args):
        pass


if __name__ == "__main__":
    port = 8099
    print(f"Dev Cluster API on http://localhost:{port}/api/clusters")
    HTTPServer(("", port), Handler).serve_forever()
