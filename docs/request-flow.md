# Request Flow — End to End

## Happy Path: Chat Completion Request

```
1. Client sends POST /v1/chat/completions
   Headers: Authorization: Bearer sk-abc123...
   Body: { model: "llama-70b", messages: [...], stream: true }

2. Cloud Load Balancer → terminates TLS → forwards to Envoy Gateway

3. Envoy Gateway
   ├─ Extracts API key from Authorization header
   ├─ Calls Tenant Manager /api/auth/verify
   │   └─ Returns: tenant_id, allowed_models, rate_limit
   ├─ Checks rate limit (per-tenant token bucket)
   ├─ Validates model is in tenant's allowed_models
   ├─ Adds headers: X-Tenant-ID, X-Session-ID
   └─ Calls ext_proc → Smart Router

4. Smart Router receives request
   ├─ Computes prefix_hash = sha256(prompt[:128])
   ├─ Checks Session Router (Redis)
   │   └─ session_id → cluster_id? → if yes, score += 0.20
   ├─ Checks KV Cache Registry (Redis)
   │   └─ prefix_hash → cluster_id? → if yes, score += 0.50
   ├─ Queries cluster load from Cluster Manager
   │   └─ (1 - load_ratio) * 0.20
   ├─ Checks latency from cluster metadata
   │   └─ (200 - latency) / 200 * 0.10
   ├─ Selects cluster with highest score
   ├─ Pins session → cluster in Redis
   └─ Returns X-Route-Cluster header to Envoy

5. Envoy routes request to selected cluster's ingress endpoint

6. Cluster Agent's local Envoy / vLLM router
   ├─ Receives request
   ├─ Checks per-pod KV cache occupancy
   ├─ Checks per-pod queue depth
   └─ Routes to optimal inference pod

7. vLLM Inference Pod
   ├─ API Server: tokenizes prompt
   ├─ Engine Core: schedules into batch
   │   ├─ Checks prefix cache (PagedAttention)
   │   │   └─ Cache hit → skip prefill for cached tokens
   │   └─ Allocates KV cache blocks
   ├─ GPU Worker: runs model forward pass
   ├─ Generates tokens one by one
   └─ Streams SSE response back through the chain

8. Response flows back:
   Pod → Cluster Agent → Envoy Gateway → Client (SSE stream)

9. Post-request:
   ├─ Smart Router logs: tenant, model, latency, tokens
   ├─ Tenant Manager records usage
   ├─ Prometheus scrapes metrics
   └─ Cluster Agent reports updated prefix hashes
```

## Failure Scenarios

### Cluster goes offline
```
Cluster Agent stops sending heartbeats
    → Cluster Manager marks cluster "degraded" (30s) then "offline" (60s)
    → Smart Router excludes offline clusters from scoring
    → Existing sessions are re-routed to next-best cluster
    → KV cache entries for that cluster expire (60s TTL)
```

### Pod OOM / Crash
```
Pod crashes
    → Kubernetes restarts pod (restartPolicy: Always)
    → HPA detects reduced ready replicas
    → If queue grows, HPA scales up additional pods
    → Local router stops sending traffic to unready pod
    → New pod loads model and joins rotation
```

### All pods for a model are at capacity
```
Queue depth exceeds threshold
    → KEDA ScaledObject triggers scale-up
    → If no GPU nodes available, Cluster Autoscaler provisions new node
    → Cold-start mitigation: warm pool pods serve initial burst
    → If BYOC cluster available with capacity, Smart Router shifts traffic there
```

### DynamoDB throttling
```
High burst of metadata writes
    → DynamoDB on-demand auto-scales (takes ~minutes)
    → Control plane services use local cache for tenant config (30s TTL)
    → Non-critical writes (usage logs) are queued in memory and retried
```

## Latency Budget

| Stage | Target | Notes |
|-------|--------|-------|
| TLS + Gateway | <5ms | Envoy is fast |
| Auth verification | <10ms | Redis-cached tenant data |
| Smart Router decision | <5ms | In-memory scoring + Redis |
| Network to cluster | <20ms | Same region; higher for cross-region |
| vLLM scheduling | <5ms | Continuous batching |
| First token (prefill) | 50-500ms | Depends on prompt length + GPU |
| Per-token decode | 10-30ms | Depends on model + batch size |
| **Total to first token** | **~100-550ms** | |
