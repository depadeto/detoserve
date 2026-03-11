"""
Lightweight cluster discovery API for the DetoServe dev environment.
Queries the local Kubernetes cluster for node/GPU information and
serves it as JSON for the frontend dashboard.
"""
import json
import subprocess
from http.server import HTTPServer, BaseHTTPRequestHandler


def get_cluster_info():
    result = subprocess.run(
        ["kubectl", "get", "nodes", "-o", "json"],
        capture_output=True, text=True
    )
    if result.returncode != 0:
        return {"error": result.stderr}

    data = json.loads(result.stdout)
    cluster_name = "detoserve-dev"

    try:
        ctx = subprocess.run(
            ["kubectl", "config", "current-context"],
            capture_output=True, text=True
        )
        cluster_name = ctx.stdout.strip()
    except Exception:
        pass

    nodes = []
    total_gpus = 0
    total_gpus_available = 0

    for node in data.get("items", []):
        meta = node["metadata"]
        labels = meta.get("labels", {})
        status = node.get("status", {})
        capacity = status.get("capacity", {})
        allocatable = status.get("allocatable", {})

        gpu_capacity = int(capacity.get("nvidia.com/gpu", "0"))
        gpu_allocatable = int(allocatable.get("nvidia.com/gpu", "0"))

        conditions = status.get("conditions", [])
        ready = any(
            c["type"] == "Ready" and c["status"] == "True"
            for c in conditions
        )

        roles = []
        for k in labels:
            if k.startswith("node-role.kubernetes.io/"):
                roles.append(k.split("/")[1])

        node_info = {
            "name": meta["name"],
            "status": "Ready" if ready else "NotReady",
            "roles": roles if roles else ["worker"],
            "gpu": {
                "count": gpu_capacity,
                "available": gpu_allocatable,
                "family": labels.get("nvidia.com/gpu.family", ""),
                "machine": labels.get("nvidia.com/gpu.machine", ""),
                "pool": labels.get("run.ai/simulated-gpu-node-pool", ""),
                "driver": labels.get("nvidia.com/cuda.driver.major", ""),
            },
            "cpu": capacity.get("cpu", "0"),
            "memory": capacity.get("memory", "0"),
            "k8s_version": status.get("nodeInfo", {}).get("kubeletVersion", ""),
            "os": status.get("nodeInfo", {}).get("osImage", ""),
            "container_runtime": status.get("nodeInfo", {}).get("containerRuntimeVersion", ""),
        }
        nodes.append(node_info)
        total_gpus += gpu_capacity
        total_gpus_available += gpu_allocatable

    gpu_nodes = [n for n in nodes if n["gpu"]["count"] > 0]
    gpu_types = {}
    for n in gpu_nodes:
        machine = n["gpu"]["machine"] or "Unknown"
        if machine not in gpu_types:
            gpu_types[machine] = {"count": 0, "available": 0, "family": n["gpu"]["family"]}
        gpu_types[machine]["count"] += n["gpu"]["count"]
        gpu_types[machine]["available"] += n["gpu"]["available"]

    return {
        "cluster": {
            "name": cluster_name,
            "provider": "k3d (local dev)",
            "status": "healthy",
            "k8s_version": nodes[0]["k8s_version"] if nodes else "",
            "node_count": len(nodes),
            "gpu_node_count": len(gpu_nodes),
            "total_gpus": total_gpus,
            "available_gpus": total_gpus_available,
            "gpu_types": gpu_types,
        },
        "nodes": nodes,
    }


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/api/clusters":
            data = get_cluster_info()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(json.dumps(data, indent=2).encode())
        else:
            self.send_response(404)
            self.end_headers()

    def do_OPTIONS(self):
        self.send_response(200)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, OPTIONS")
        self.end_headers()

    def log_message(self, format, *args):
        pass


if __name__ == "__main__":
    port = 8099
    print(f"Cluster API running on http://localhost:{port}/api/clusters")
    HTTPServer(("", port), Handler).serve_forever()
