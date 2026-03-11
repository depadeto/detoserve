# Cluster Onboarding Guide

## Overview

Any Kubernetes cluster with GPU nodes can join the DetoServe platform as a compute pool.
When you onboard a cluster, the **DetoServe Agent** is deployed via Helm. It automatically
discovers GPUs using the Kubernetes API (and SkyPilot when available), then continuously
reports cluster state to the control plane via heartbeats.

| Type | Description | Connection |
|------|-------------|------------|
| Internal | Platform-owned GPU clusters | Direct (same VPC) |
| BYOC | Customer-managed GPU clusters | VPN / VPC peering |
| Spot | Ephemeral spot/preemptible GPUs | Direct (same VPC) |
| Local Dev | k3d/kind with fake GPUs | localhost |

## Prerequisites

- Kubernetes 1.28+
- NVIDIA GPU Operator installed (or manual device plugin + node labels)
- Network connectivity to control plane (direct or VPN)
- Helm 3.x

## Step 1: Install the DetoServe Agent

```bash
helm install detoserve-agent ./charts/detoserve-agent \
  --namespace detoserve-system --create-namespace \
  --set controlPlane.url=https://api.detoserve.ai \
  --set cluster.id=prod-east-1 \
  --set cluster.name="Production East" \
  --set controlPlane.apiToken=<your-token>
```

The Helm chart deploys:
- A **Deployment** (1 replica) running the agent binary
- A **ServiceAccount** with RBAC to read nodes and pods
- A **ClusterRole/Binding** for GPU and node discovery
- A **Service** exposing the agent's health/status endpoints

### Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `controlPlane.url` | `https://api.detoserve.ai` | Control plane URL to send heartbeats |
| `controlPlane.apiToken` | `""` | Bearer token for authentication |
| `cluster.id` | Auto-detected | Unique cluster identifier |
| `cluster.name` | Same as ID | Human-friendly cluster name |
| `reportIntervalSec` | `10` | Heartbeat interval in seconds |
| `image.repository` | `detoserve/agent` | Agent container image |
| `image.tag` | `latest` | Image tag |

## Step 2: Agent Discovery & Heartbeat

On startup, the agent automatically:

1. **Detects cluster ID** from `kubectl config current-context`
2. **Queries all nodes** via `kubectl get nodes -o json`
3. **Extracts GPU info** from node labels and `nvidia.com/gpu` resources:
   - `nvidia.com/gpu.machine` — GPU model (e.g., `NVIDIA-A100-SXM4-80GB`)
   - `nvidia.com/gpu.family` — GPU generation (e.g., `ampere`, `hopper`)
   - `nvidia.com/gpu` capacity and allocatable counts
4. **Sends heartbeat** to control plane every N seconds:

```
POST https://api.detoserve.ai/api/clusters/heartbeat
{
  "cluster_id": "prod-east-1",
  "cluster_name": "Production East",
  "status": "healthy",
  "provider": "AWS EKS",
  "k8s_version": "v1.30.2",
  "total_gpus": 32,
  "available_gpus": 28,
  "gpu_types": [
    {"name": "NVIDIA-A100-SXM4-80GB", "family": "ampere", "count": 32, "available": 28}
  ],
  "nodes": [
    {
      "name": "gpu-node-0",
      "status": "Ready",
      "role": "worker",
      "cpu": "96",
      "memory_gb": "768.0",
      "gpu_type": "NVIDIA-A100-SXM4-80GB",
      "gpu_count": 8,
      "gpu_available": 7,
      "gpu_family": "ampere"
    }
  ]
}
```

## Step 3: Verify on Dashboard

The cluster appears on the DetoServe dashboard within seconds:

```bash
# Via API
curl https://api.detoserve.ai/api/clusters

# Expected: cluster listed with status "healthy"
```

Or open the frontend and click the **Clusters** tab to see:
- Cluster summary (total GPUs, available, in use)
- GPU type breakdown with utilization bars
- Per-node detail with GPU counts and health status

## Step 4: Deploy Functions

Once a cluster is reporting, it becomes a deployment target:

```bash
# Via the DetoServe UI or API
POST /api/functions/{id}/instances
{
  "name": "llama-70b-east",
  "cluster_id": "prod-east-1",
  "gpu_count": 4,
  "min_replicas": 2,
  "max_replicas": 10
}
```

## BYOC (Bring Your Own Cluster)

### Network Requirements

The agent only needs **outbound HTTPS** to the control plane.
No inbound access to the customer cluster is required.

Options:
- **WireGuard VPN** — lightweight, works anywhere
- **VPC Peering** — AWS/GCP/Azure native
- **Direct HTTPS** — if control plane is publicly accessible

### Security

- Agent uses bearer token or mTLS for control plane auth
- ServiceAccount has minimal RBAC (read-only for nodes/pods)
- No sensitive data leaves the cluster — inference happens locally
- Customer retains full cluster admin access

### Isolation

- Workloads run in tenant-specific namespaces
- NetworkPolicies restrict east-west traffic
- Model weights can be pulled from customer's own storage

## Local Development

For local testing with simulated GPUs:

```bash
# Create k3d cluster with GPU labels
k3d cluster create detoserve-dev --config dev/k3d-config.yaml

# Patch nodes with fake GPU resources
kubectl proxy &
curl -X PATCH http://localhost:8001/api/v1/nodes/<node>/status \
  -H "Content-Type: application/json-patch+json" \
  -d '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"8"},
       {"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"8"}]'

# Run the dev cluster API (simulates agent → control plane)
python3 dev/cluster-api.py

# Start the frontend
cd frontend && npm run dev
```

## Agent RBAC

The agent needs read-only access to cluster resources:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: detoserve-agent
rules:
  - apiGroups: [""]
    resources: ["nodes", "pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["nodes/status"]
    verbs: ["get"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["nodes", "pods"]
    verbs: ["get", "list"]
```

## Monitoring

Once onboarded, the agent:
- Sends heartbeats every 10 seconds (configurable)
- Reports GPU availability from node status
- Detects provider (EKS/GKE/AKS/k3d) automatically
- Exposes `/healthz` and `/status` endpoints for debugging
- Marks clusters as "stale" on the dashboard if heartbeats stop
