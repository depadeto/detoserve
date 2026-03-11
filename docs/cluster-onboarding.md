# Cluster Onboarding Guide

## Overview

Any Kubernetes cluster with GPU nodes can join the platform as a compute pool.
Three types of clusters are supported:

| Type | Description | Connection |
|------|-------------|------------|
| Internal | Platform-owned GPU clusters | Direct (same VPC) |
| BYOC | Customer-managed GPU clusters | VPN / VPC peering |
| Spot | Ephemeral spot/preemptible GPUs | Direct (same VPC) |

## Prerequisites

- Kubernetes 1.28+
- NVIDIA GPU Operator installed (or manual device plugin)
- Network connectivity to control plane (direct or VPN)
- Helm 3.x

## Step 1: Install Cluster Agent

```bash
helm install detoserve-agent ./charts/cluster-agent \
  --set controlPlane.url=https://api.detoserve.ai \
  --set cluster.name=prod-east-1 \
  --set cluster.region=us-east-1 \
  --set cluster.provider=internal \
  --set cluster.gpuType=A100-80GB \
  --set cluster.totalGPUs=32
```

The agent creates:
- A Deployment (1 replica) in `detoserve-system` namespace
- A ServiceAccount with scoped RBAC
- A ConfigMap with cluster metadata

## Step 2: Agent Registers with Control Plane

On startup, the agent sends:

```
POST https://api.detoserve.ai/api/clusters/register
{
  "name": "prod-east-1",
  "region": "us-east-1",
  "provider": "internal",
  "endpoint": "https://10.0.1.5:6443",
  "gpu_type": "A100-80GB",
  "total_gpus": 32,
  "agent_version": "0.1.0",
  "kube_version": "1.30.2"
}
```

Control plane returns a `cluster_id` and the agent starts heartbeating.

## Step 3: Verify Registration

```bash
# From control plane
curl https://api.detoserve.ai/api/clusters

# Expected: cluster appears with status "healthy"
```

## Step 4: Deploy Models

Once registered, the cluster is available as a target for deployments:

```yaml
apiVersion: inference.detoserve.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: llama-70b
spec:
  clusters:
    - prod-east-1    # this cluster
```

The Deployment Manager sends the manifest to the Cluster Agent,
which applies it to the local Kubernetes API.

## BYOC Cluster Setup

### Network

Customer must establish one of:
- **WireGuard VPN** — lightweight, works anywhere
- **VPC Peering** — AWS/GCP/Azure native peering
- **Private Link** — cloud-specific private endpoint

The agent only needs outbound HTTPS to the control plane.
No inbound access is required.

### Security

- Agent uses mTLS certificates for control plane communication
- Agent ServiceAccount has minimal RBAC (create/update pods in specific namespaces)
- Customer retains full cluster admin access
- No sensitive data leaves the customer cluster (inference happens locally)

### Isolation

- BYOC cluster pods run in tenant-specific namespaces
- NetworkPolicies restrict east-west traffic
- Model weights can be pulled from customer's own storage (S3/GCS)

## Agent RBAC

The cluster agent needs these permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: detoserve-agent
rules:
  - apiGroups: [""]
    resources: ["namespaces", "pods", "services", "configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["apps"]
    resources: ["deployments", "daemonsets"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["autoscaling"]
    resources: ["horizontalpodautoscalers"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["keda.sh"]
    resources: ["scaledobjects"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["networkpolicies"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["nodes", "pods"]
    verbs: ["get", "list"]
```

## Monitoring

Once onboarded, the cluster agent:
- Sends heartbeats every 10s
- Reports GPU availability via DCGM exporter metrics
- Reports vLLM queue depth and prefix cache state
- Exposes a `/metrics` endpoint for Prometheus federation
