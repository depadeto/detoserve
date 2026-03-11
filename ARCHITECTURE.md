# DetoServe — Architecture

## Overview

Multi-cluster AI inference platform **built on top of SkyPilot** that adds
multi-tenant smart routing, KV-cache-aware request dispatch, and a unified
OpenAI-compatible API gateway across distributed GPU clusters.

**SkyPilot handles:** cluster provisioning, service deployment, GPU scheduling,
spot recovery, cost optimization, multi-cloud abstraction.

**We build on top:** tenant isolation, cache-aware smart routing, per-request
dispatch, usage tracking, and the unified API surface.

---

## Why Build on SkyPilot

| Problem | Without SkyPilot (custom) | With SkyPilot |
|---------|--------------------------|---------------|
| Cluster provisioning | Build Cluster Manager + Agent | `sky launch` — done |
| Multi-cloud support | Integrate each cloud API | 20+ clouds + K8s + on-prem |
| vLLM deployment | Write K8s manifests manually | `sky serve` with YAML |
| Spot GPU recovery | Build preemption handler | Built-in auto-recovery |
| Autoscaling replicas | Build HPA/KEDA integration | `sky serve` auto-scales |
| Cost optimization | Build cost comparator | Automatic cheapest selection |
| Cluster lifecycle | Build autostop, teardown | Built-in autostop |
| Warm pools | Build warm pool manager | SkyPilot Pools (v0.11) |

SkyPilot eliminates ~60% of the custom infrastructure code, letting us
focus on what matters: **intelligent request routing and multi-tenancy**.

---

## 4-Layer Architecture

```
                         Clients / Apps / Agents
                                  │
                                  ▼
┌──────────────────────────────────────────────────────────┐
│  Layer 1 — EDGE (You Build)                              │
│  Envoy API Gateway (Gateway API)                         │
│  TLS · Auth · Rate-limit · OpenAI-compat API             │
└──────────────────────┬───────────────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────────────┐
│  Layer 2 — SMART CONTROL PLANE (You Build)               │
│                                                          │
│  ┌──────────┐ ┌──────────────────────────────────┐       │
│  │ Tenant   │ │ Smart Router                      │       │
│  │ Manager  │ │  ├─ Session Router (Redis)         │       │
│  └──────────┘ │  ├─ KV Cache Router (Redis)        │       │
│               │  ├─ Cluster Scorer (load+latency)  │       │
│  ┌──────────┐ │  └─ Semantic Router (Phase 4)      │       │
│  │ SkyPilot │ └──────────────────────────────────┘       │
│  │ Bridge   │ ┌──────────────────────────────────┐       │
│  │ (wrapper)│ │ Cache Registry (Redis)            │       │
│  └──────────┘ └──────────────────────────────────┘       │
│  ┌──────────┐ ┌──────────────────────────────────┐       │
│  │ Metadata │ │ Usage Tracker / Billing           │       │
│  │ (DynamoDB)│ └──────────────────────────────────┘       │
│  └──────────┘                                            │
└──────────────────────┬───────────────────────────────────┘
                       │
═══════════════════════╪═══════════════════════════════════
   SkyPilot takes over below this line
═══════════════════════╪═══════════════════════════════════
                       │
┌──────────────────────▼───────────────────────────────────┐
│  Layer 3 — SKYPILOT INFRASTRUCTURE                       │
│                                                          │
│  SkyPilot Control Plane                                  │
│  ├─ sky serve   → deploy + scale vLLM/Triton services    │
│  ├─ sky launch  → provision GPU clusters on any cloud    │
│  ├─ sky jobs    → batch inference workloads               │
│  ├─ Pools       → warm worker pools across clouds        │
│  ├─ Optimizer   → cheapest GPU across 20+ clouds         │
│  └─ Spot Mgr    → preemption recovery + fallback         │
│                                                          │
│  SkyPilot manages clusters:                              │
│  ┌────────────┐ ┌────────────┐ ┌────────────┐           │
│  │ Cluster A  │ │ Cluster B  │ │ Cluster C  │           │
│  │ (K8s/EKS)  │ │ (GCP Spot) │ │ (On-prem)  │           │
│  └────────────┘ └────────────┘ └────────────┘           │
└──────────────────────┬───────────────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────────────┐
│  Layer 4 — GPU RUNTIME                                   │
│                                                          │
│  vLLM Pods / LLM-D Pods / Triton Pods / Dynamo Pods      │
│  ├─ API Server (OpenAI compat)                           │
│  ├─ Engine Core (PagedAttention, continuous batching)     │
│  ├─ GPU Workers (model forward pass)                     │
│  └─ Local router (per-pod queue, KV cache occupancy)     │
└──────────────────────────────────────────────────────────┘
```

---

## Layer 1 — Edge (API Gateway)

**Component:** Envoy Proxy via Gateway API CRDs

**Responsibilities:**
- Single public HTTPS endpoint: `api.detoserve.ai`
- TLS termination
- API key / OAuth / OIDC authentication (calls Tenant Manager)
- Per-tenant rate limiting
- OpenAI-compatible API surface (`/v1/chat/completions`, `/v1/models`)
- Request tagging with `X-Tenant-ID`, `X-Session-ID` headers
- Streaming (SSE) pass-through to inference pods
- Ext_proc filter → Smart Router for per-request routing decisions

**Why Envoy:** Already used in the existing stack for weighted routing
(blue/green). Gateway API CRDs give declarative config.

---

## Layer 2 — Smart Control Plane

### 2.1 Tenant Manager (You Build)

Multi-tenant auth, quotas, and usage tracking. SkyPilot has no concept
of tenants — this is entirely your layer.

| Field | Example |
|-------|---------|
| tenant_id | `hospital-a` |
| api_key_hash | `sha256:...` |
| allowed_models | `[llama-70b, mixtral]` |
| gpu_quota | `8` |
| rate_limit_rps | `100` |
| namespace | `tenant-hospital-a` |

**Storage:** DynamoDB single-table design.
Partition key: `TENANT#<id>`, sort key: `CONFIG`.

### 2.2 Smart Router (You Build)

The brain of per-request routing. SkyPilot routes *services*, this routes
individual *requests* to the right cluster based on cache state.

**4-signal scoring algorithm:**

```
for each candidate_cluster:
    score = 0

    # Signal 1: KV-cache locality (highest weight)
    prefix_hash = sha256(prompt[:N_PREFIX_TOKENS])
    if cache_registry.has(prefix_hash, cluster):
        score += 0.50

    # Signal 2: Session affinity
    if session_store.get(session_id) == cluster:
        score += 0.20

    # Signal 3: Load headroom
    load_ratio = cluster.active_requests / cluster.capacity
    score += (1 - load_ratio) * 0.20

    # Signal 4: Latency / region proximity
    latency_ms = cluster.avg_latency_ms
    score += max(0, (200 - latency_ms) / 200) * 0.10

route_to(cluster_with_max_score)
```

**Sub-components:**

| Sub-component | Storage | Purpose |
|---------------|---------|---------|
| Session Router | Redis | `session_id → cluster_id` pinning |
| KV Cache Registry | Redis | `prefix_hash → cluster_id` mapping |
| Semantic Classifier | In-memory ONNX | Intent → model class (Phase 4) |
| Cluster Scorer | In-memory | Score computation per request |

**Cache Registry updates:** A lightweight metrics sidecar in each cluster
scrapes vLLM `/metrics`, extracts active prefix hashes, and POSTs them
to the Smart Router every 5s. TTL-based eviction (60s).

### 2.3 SkyPilot Bridge (You Build)

Thin wrapper service that translates platform API calls into SkyPilot
CLI/SDK operations. This is how your platform talks to SkyPilot.

**Responsibilities:**
- Translate `POST /deployments` → `sky serve up` with correct YAML
- Query `sky status` / `sky serve status` for cluster/service state
- Feed cluster metadata to Smart Router (endpoints, GPU counts, models)
- Trigger `sky serve update` for scaling/config changes
- Manage SkyPilot Pools for warm capacity

**Why a bridge:** SkyPilot has a Python SDK and CLI. Your control plane
is Go. The bridge is a Python FastAPI service that wraps SkyPilot SDK
calls and exposes them as REST endpoints for the Go services.

### 2.4 Metadata Store (DynamoDB)

Single-table design:

| PK | SK | Data |
|----|-----|------|
| `TENANT#hospital-a` | `CONFIG` | quota, models, rate_limit |
| `TENANT#hospital-a` | `USAGE#2026-03` | tokens, cost, requests |
| `SERVICE#llama-70b` | `CLUSTER#prod-east-1` | endpoint, replicas, status |
| `SERVICE#llama-70b` | `CLUSTER#byoc-west-2` | endpoint, replicas, status |
| `REQUEST#req-456` | `LOG` | tenant, model, latency, tokens |

---

## Layer 3 — SkyPilot Infrastructure

### What SkyPilot Manages

SkyPilot replaces what was previously Cluster Manager, Cluster Agent,
Deployment Manager, and Autoscaler Controller.

#### Cluster Provisioning

```bash
# Provision a GPU cluster on AWS
sky launch --gpus A100:8 --cloud aws --region us-east-1

# Or use an existing K8s cluster
sky check    # auto-detects kubeconfig
```

SkyPilot supports: Kubernetes, AWS, GCP, Azure, OCI, Lambda, RunPod,
Nebius, CoreWeave, and 10+ more providers.

#### Service Deployment (sky serve)

Instead of writing raw K8s Deployments + Services + HPAs, you define
a SkyPilot service YAML:

```yaml
# llama-70b.yaml
service:
  replicas: 2
  readiness_probe:
    path: /health
    initial_delay_seconds: 120

resources:
  accelerators: A100:4
  cloud: aws
  region: us-east-1

setup: |
  pip install vllm

run: |
  python -m vllm.entrypoints.openai.api_server \
    --model meta-llama/Llama-3.1-70B-Instruct \
    --tensor-parallel-size 4 \
    --enable-prefix-caching \
    --max-model-len 8192 \
    --port 8000
```

Deploy:

```bash
sky serve up llama-70b.yaml -n llama-70b
```

SkyPilot handles:
- Finding cheapest available GPUs
- Provisioning VMs/pods
- Setting up the environment
- Health checks and auto-recovery
- Replica scaling

#### Autoscaling

SkyPilot `sky serve` supports replica autoscaling:

```yaml
service:
  replicas:
    min: 2
    max: 20
  # SkyPilot auto-scales based on request load
```

For custom metrics (queue depth, GPU util), we supplement with the
Smart Router's load data — the SkyPilot Bridge can call
`sky serve update` to adjust replica counts.

#### Spot Instance Recovery

```yaml
resources:
  accelerators: A100:4
  use_spot: true
  # SkyPilot auto-recovers on preemption
```

SkyPilot automatically finds replacement capacity on preemption,
even across different clouds if configured.

#### Warm Pools (v0.11)

```bash
sky pool create --gpus A100:4 --size 3 --name inference-pool
```

Maintains warm workers ready to serve, reducing cold-start to ~seconds.

### What SkyPilot Does NOT Handle (Your Layer)

| Concern | SkyPilot | Your Layer |
|---------|----------|------------|
| Per-request routing | No | Smart Router |
| KV cache awareness | No | Cache Registry + Router |
| Multi-tenant auth | No | Tenant Manager |
| Session pinning | No | Session Router (Redis) |
| Per-tenant billing | No | Usage Tracker |
| Unified API gateway | No | Envoy Gateway |

---

## Layer 4 — GPU Runtime

Same as before — runs inside whatever SkyPilot provisions:

### vLLM Stack (per pod/VM)

```
Pod
├─ API Server      (HTTP, tokenization, OpenAI compat)
├─ Engine Core     (scheduling, PagedAttention)
└─ GPU Worker(s)   (model forward pass, 1 per GPU)
```

### LLM-D / Dynamo Stack

```
Pod
├─ API Server
├─ Prefill Engine   (prompt processing)
├─ Decode Engine    (token generation)
└─ KV Transfer      (cache migration between pods)
```

### Triton Stack (existing pattern — Whisper/TTS)

```
Pod
├─ Triton Server
├─ Model Repository (mounted via NFS/PVC)
└─ GPU(s)
```

---

## Request Flow — End to End

```
1. Client → POST /v1/chat/completions
   Headers: Authorization: Bearer sk-abc...

2. Envoy Gateway
   ├─ Extract API key → call Tenant Manager /api/auth/verify
   ├─ Check rate limit (per-tenant)
   ├─ Add X-Tenant-ID, X-Session-ID headers
   └─ ext_proc → Smart Router

3. Smart Router
   ├─ Compute prefix_hash = sha256(prompt[:128])
   ├─ Redis: check SESSION:<id> → cluster_id?
   ├─ Redis: check PREFIX:<hash> → cluster_id?
   ├─ Score clusters (cache + session + load + latency)
   ├─ Select best cluster
   ├─ Pin session → cluster in Redis
   └─ Return X-Route-Cluster header to Envoy

4. Envoy routes to SkyPilot service endpoint on selected cluster

5. vLLM Pod
   ├─ Tokenize → schedule → prefill → decode
   └─ Stream SSE response back

6. Response → Client (SSE stream)

7. Post-request:
   ├─ Smart Router logs: tenant, model, latency, tokens
   ├─ Tenant Manager records usage
   └─ Cache sidecar reports prefix hashes to Smart Router
```

---

## Networking

```
Internet
    │
    ▼
Cloud LB (TLS termination)
    │
    ▼
Envoy Gateway pods (public subnet)
    │
    ├─── Smart Router + Tenant Manager (private subnet)
    │
    ├─── SkyPilot API Server (private subnet)
    │
    ├─── SkyPilot-managed clusters
    │    ├── K8s clusters (direct)
    │    ├── Cloud VMs (SkyPilot provisioned)
    │    └── BYOC clusters (VPN/peering)
    │
    └─── Redis Cluster (private subnet)
```

**Tenant isolation:**
- Kubernetes Namespace per tenant (on K8s clusters)
- SkyPilot service isolation (separate services per tenant/model)
- DynamoDB: IAM `dynamodb:LeadingKeys` condition on `TENANT#X`
- NetworkPolicy where applicable

---

## Observability

**Stack:** OpenTelemetry → Prometheus → Grafana + Jaeger

| Signal | Collector | Storage | Dashboard |
|--------|-----------|---------|-----------|
| Request latency | OTel SDK in Smart Router | Prometheus | Grafana |
| Tokens/sec | vLLM metrics | Prometheus | Grafana |
| GPU utilization | DCGM exporter | Prometheus | Grafana |
| Queue depth | vLLM metrics | Prometheus | Grafana |
| KV cache hit rate | Smart Router | Prometheus | Grafana |
| Cost per request | SkyPilot + custom | DynamoDB | Grafana |
| Distributed traces | OTel + Jaeger | Jaeger | Jaeger UI |
| SkyPilot status | `sky status` / `sky serve status` | — | CLI/API |

**Key alerts:**
- `rate(inference_latency_sum[5m]) / rate(inference_latency_count[5m]) > 0.5`
- `avg_over_time(nvidia_gpu_utilization[1m]) > 0.9` for 5m
- `vllm_request_queue_size > 50` for 2m
- Smart Router cache hit rate < 30%

---

## What You Build vs What SkyPilot Provides

```
┌──────────────────────────────────────────────────────────┐
│                    YOU BUILD                              │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ API Gateway  │  │ Smart Router │  │ Tenant Mgr   │  │
│  │ (Envoy)      │  │ (Go+Redis)   │  │ (Go+DynamoDB)│  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ SkyPilot     │  │ Cache        │  │ Usage/Billing│  │
│  │ Bridge (Py)  │  │ Registry     │  │ Tracker      │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│                                                          │
│  ┌──────────────┐                                       │
│  │ Cache        │  (sidecar in each cluster, scrapes    │
│  │ Reporter     │   vLLM /metrics, reports prefix       │
│  │ (sidecar)    │   hashes to Smart Router)             │
│  └──────────────┘                                       │
└──────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────┐
│                  SKYPILOT PROVIDES                        │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Cluster      │  │ Service      │  │ Autoscaling  │  │
│  │ Provisioning │  │ Deployment   │  │ + Recovery   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Multi-cloud  │  │ Spot GPU     │  │ Warm Pools   │  │
│  │ Abstraction  │  │ Management   │  │ (v0.11)      │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐                     │
│  │ Cost         │  │ GPU          │                     │
│  │ Optimizer    │  │ Scheduling   │                     │
│  └──────────────┘  └──────────────┘                     │
└──────────────────────────────────────────────────────────┘
```

---

## SkyPilot Service Templates

### vLLM Serving

```yaml
# services/vllm-llama70b.yaml
service:
  replicas:
    min: 2
    max: 20
  readiness_probe:
    path: /health
    initial_delay_seconds: 120

resources:
  accelerators: A100:4
  memory: 128+
  disk_size: 256

setup: |
  pip install vllm

run: |
  python -m vllm.entrypoints.openai.api_server \
    --model meta-llama/Llama-3.1-70B-Instruct \
    --tensor-parallel-size 4 \
    --enable-prefix-caching \
    --enable-chunked-prefill \
    --max-model-len 8192 \
    --port 8000
```

### Triton Serving (Whisper/TTS)

```yaml
# services/triton-whisper.yaml
service:
  replicas:
    min: 1
    max: 8
  readiness_probe:
    path: /v2/health/ready
    initial_delay_seconds: 60

resources:
  accelerators: A100:1
  image_id: appsprodacr.azurecr.io/inference/flash/deployment-trt-flash-release:v0.17

run: |
  tritonserver \
    --model-repository=/models \
    --http-port=8000
```

### Dynamo / LLM-D (Disaggregated)

```yaml
# services/dynamo-mixtral.yaml
service:
  replicas:
    min: 2
    max: 16
  readiness_probe:
    path: /health
    initial_delay_seconds: 180

resources:
  accelerators: A100:4
  memory: 256+

setup: |
  pip install nvidia-dynamo

run: |
  dynamo serve \
    --model mistralai/Mixtral-8x7B-Instruct-v0.1 \
    --disaggregated-prefill \
    --kv-transfer-method nccl \
    --prefill-tp 4 \
    --decode-tp 2 \
    --port 8000
```

### Multi-cloud with Spot Fallback

```yaml
# services/vllm-spot.yaml
service:
  replicas: 4

resources:
  accelerators: {A100:4, H100:4}   # either works
  use_spot: true
  # SkyPilot tries spot first, falls back to on-demand
  # Tries across clouds: AWS → GCP → Azure → Lambda
```

---

## Functions — Define Once, Deploy Anywhere

A **Function** is a reusable deployment template. Define the model,
runtime, GPU resources, and scaling policy once. Then create multiple
**Instances** of that function across different clusters, tenants,
regions, or clouds.

```
Function (blueprint)
├── name: llama-70b-chat
├── runtime: vllm
├── model: meta-llama/Llama-3.1-70B-Instruct
├── gpu: 4x A100
├── scaling: 2-20 replicas on queue_depth
│
├── Instance 1 → tenant-hospital-a / prod-east-1 / on-demand
├── Instance 2 → tenant-analytics / byoc-west-2 / spot
└── Instance 3 → tenant-research / gcp-europe / on-demand
```

### Function Manager API

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/functions` | POST | Create a function |
| `/api/functions` | GET | List all functions |
| `/api/functions/:id` | GET | Get function details |
| `/api/functions/:id` | PUT | Update function config |
| `/api/functions/:id/deploy` | POST | Deploy a new instance |
| `/api/instances` | GET | List all instances |
| `/api/instances/:id` | DELETE | Undeploy an instance |

### Frontend UI

React-based dashboard at `frontend/` for managing functions:
- Create functions with form (model URI, GPU type, scaling, routing)
- View all functions as cards
- Deploy instances to any cluster/cloud/region
- Monitor instance status (deploying/running/failed)
- One-click undeploy

---

## GitOps Config Store

Every function definition and deployment config is automatically saved
to disk as JSON files. The storage directory is designed to be a Git repo.

```
/data/configs/
├── functions/
│   ├── fn-123456.json      # Llama 70B function definition
│   ├── fn-789012.json      # Whisper ASR function definition
│   └── fn-345678.json      # Mixtral function definition
└── instances/
    ├── inst-111111.json     # Running on prod-east-1
    └── inst-222222.json     # Running on byoc-west-2
```

### GitOps Flow

```
1. User creates function via UI or API
2. Function Manager saves to Config Store
3. Config Store writes JSON to disk (/data/configs/)
4. Git commit + push (triggered via API or cron)
5. ArgoCD watches the repo
6. On next sync, ArgoCD applies any changes
7. Full audit trail via git log
8. Rollback = git revert + ArgoCD sync
```

### Why This Matters

- **Persistence:** functions survive service restarts
- **Audit:** git log shows who changed what and when
- **Rollback:** git revert restores any previous config
- **Reproducibility:** clone the repo to recreate the entire platform
- **GitOps native:** ArgoCD/Flux can reconcile from the config repo

---

## KAI Scheduler — GPU Scheduling

[KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) is
installed inside each Kubernetes cluster to handle GPU pod scheduling.

### Production Readiness: YES

| Signal | Status |
|--------|--------|
| CNCF Sandbox project | Yes |
| LTS releases | v0.12 LTS → supported until Dec 2026 |
| Latest release | v0.13.1 (March 2026) |
| KubeCon presentation | KubeCon NA 2025 |
| Dynamo/Grove integration | Yes (hierarchical PodGroups) |
| Multi-cluster (roadmap) | Planned for 2026 |
| Stars / Contributors | 1.2k stars, active community |

### What KAI Gives Us

| Feature | Why It Matters |
|---------|---------------|
| Topology-aware scheduling | 4x A100 placed on same NVLink domain |
| Gang scheduling | All GPUs allocated or none (no partial) |
| Hierarchical queues | Per-tenant GPU quotas with fairshare |
| GPU sharing | Multiple small models on one GPU |
| Elastic workloads | Scale pods within min/max dynamically |
| Time-based fairshare | Fair GPU access over time, not just instant |
| DRA support | NVIDIA ComputeResources (GB200/GB300) |

### How It Integrates

```
SkyPilot provisions cluster
    ↓
KAI Scheduler installed via Helm
    ↓
Inference pods use schedulerName: kai-scheduler
    ↓
KAI handles GPU topology + gang scheduling
    ↓
Smart Router is unaware of KAI (different layer)
```

KAI operates at Layer 4 (GPU runtime). The Smart Router operates at
Layer 2 (cluster selection). They don't overlap — KAI decides which
GPUs on which node, the Smart Router decides which cluster.

### Per-Tenant Queue Setup

```yaml
Root Queue (64 GPUs total)
├── tenant-hospital-a  (quota: 8, limit: 16, overQuotaWeight: 1)
├── tenant-analytics   (quota: 16, limit: 32, overQuotaWeight: 2)
└── shared-pool        (quota: 0, limit: 64, overQuotaWeight: 1)
```

Tenants get guaranteed GPU quota. When other tenants are idle, they
can burst beyond quota. KAI's fairshare ensures equitable access.

---

## Implementation Phases (Revised)

### Phase 1 — Foundation (Weeks 1-2)
- [ ] Install SkyPilot, connect to existing K8s clusters
- [ ] Deploy vLLM via `sky serve` (validate it works)
- [ ] Build Function Manager + Config Store
- [ ] Build Tenant Manager (API keys, auth, quotas)
- [ ] Build frontend UI
- [ ] Build SkyPilot Bridge (Python wrapper exposing REST)

### Phase 2 — Smart Routing (Weeks 3-4)
- [ ] Build Smart Router (Go + Redis)
- [ ] KV Cache Registry + Cache Reporter sidecar
- [ ] Session Router (Redis)
- [ ] API Gateway (Envoy + ext_proc → Smart Router)
- [ ] Install KAI Scheduler on GPU clusters

### Phase 3 — Observability + Production (Weeks 5-6)
- [ ] Prometheus federation for cross-cluster metrics
- [ ] Grafana dashboards (per-tenant, per-model, per-cluster)
- [ ] OTel tracing through full request path
- [ ] Usage tracking + per-tenant billing in DynamoDB
- [ ] GitOps: connect Config Store to Git repo + ArgoCD

### Phase 4 — Advanced (Weeks 7-9)
- [ ] Semantic Router (intent → model class)
- [ ] Dynamo/LLM-D with KAI hierarchical PodGroups
- [ ] BYOC cluster onboarding (VPN + SkyPilot K8s connector)
- [ ] Canary deployments (SkyPilot rolling update)
- [ ] Multi-cloud failover policies

---

## Technology Stack

| Layer | Technology |
|-------|-----------|
| API Gateway | Envoy + Gateway API |
| Smart Router | Go (Fiber) + Redis |
| Function Manager | Go (Fiber) |
| Config Store | Go (Fiber) + filesystem/Git |
| Tenant Manager | Go (Fiber) + DynamoDB |
| SkyPilot Bridge | Python (FastAPI) + SkyPilot SDK |
| Cache Reporter | Go sidecar (scrapes vLLM /metrics) |
| Metadata store | DynamoDB (single-table) |
| Cache + Session store | Redis Cluster |
| Infrastructure | **SkyPilot** (provisioning, deployment, scaling) |
| GPU Scheduling | **KAI Scheduler** (topology, gang, fairshare) |
| Inference engines | vLLM, LLM-D/Dynamo, Triton |
| Frontend | React + Vite |
| Observability | OTel + Prometheus + Grafana + Jaeger |
| CI/CD | ArgoCD + GitHub Actions |

---

## Key Design Decisions

1. **Build on SkyPilot, not from scratch** — SkyPilot handles the hard
   infrastructure problems (multi-cloud, spot, scaling, provisioning).
   We focus on what it doesn't do: per-request smart routing and tenancy.

2. **Functions as first-class abstraction** — Define once, deploy many.
   Decouples "what to run" from "where to run it." Each function
   definition is persisted in the Config Store for GitOps.

3. **KAI Scheduler for GPU-level scheduling** — Topology-aware placement,
   gang scheduling, and per-tenant fairshare. SkyPilot provisions
   clusters; KAI schedules GPUs within them.

4. **Smart Router in control plane, not in gateway** — Envoy ext_proc
   calls the router service. Router has Redis access for cache/session
   state. Gateway remains stateless and horizontally scalable.

5. **Two-layer routing** — Global Smart Router picks *cluster* based on
   coarse signals (cache, session, load). Local vLLM router inside each
   cluster picks *pod* based on fine signals (queue, GPU memory).

6. **GitOps config persistence** — Every function and instance config
   is saved to disk/git. No deployment is ever lost. ArgoCD reconciles.

7. **Cache-aware routing is the differentiator** — KV cache reuse gives
   2-5x latency improvement. No existing platform (including SkyPilot)
   does cross-cluster cache-aware request routing.
