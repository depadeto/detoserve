#!/usr/bin/env python3
"""SkyPilot GPU discovery helper — called by the Go agent.

Outputs JSON with per-node GPU info using SkyPilot's Kubernetes API.
"""
import json
import sys
import warnings
import os

warnings.filterwarnings("ignore")
os.environ["PYTHONWARNINGS"] = "ignore"

try:
    from sky.provision.kubernetes import utils as k8s_utils
except ImportError:
    print(json.dumps({"error": "skypilot not installed"}))
    sys.exit(1)

try:
    info = k8s_utils.get_kubernetes_node_info()
    data = info.to_dict()
    nodes = []
    for name, node in data.get("node_info_dict", {}).items():
        nodes.append({
            "name": name,
            "accelerator_type": node.get("accelerator_type") or "",
            "gpu_count": node.get("total", {}).get("accelerator_count", 0),
            "gpu_available": node.get("free", {}).get("accelerators_available", 0),
            "cpu": node.get("cpu_count", 0),
            "cpu_free": node.get("cpu_free", 0),
            "memory_gb": round(node.get("memory_gb", 0), 1),
            "memory_free_gb": round(node.get("memory_free_gb", 0), 1),
            "ip_address": node.get("ip_address", ""),
            "is_ready": node.get("is_ready", False),
        })
    print(json.dumps({"nodes": nodes}))
except Exception as e:
    print(json.dumps({"error": str(e)}))
    sys.exit(1)
