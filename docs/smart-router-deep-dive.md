# Smart Router — Deep Dive

## Why Two-Layer Routing

Most LLM platforms use a single routing layer. This breaks down at scale because:

1. A global router tracking every pod across every cluster creates a bottleneck
2. KV cache state changes rapidly (per-token), making global tracking stale
3. Different clusters may have different routing capabilities

Our design splits routing into two layers:

```
Global Smart Router (control plane)
    → picks the CLUSTER based on coarse signals
    → cache locality, session affinity, load, latency

Local Router (per cluster)
    → picks the POD based on fine-grained signals
    → per-pod queue, per-pod KV occupancy, GPU memory
```

## Scoring Algorithm

### Weights

| Signal | Weight | Why |
|--------|--------|-----|
| KV Cache locality | 0.50 | Reusing cache saves 2-5x latency |
| Session affinity | 0.20 | Chat continuity, state reuse |
| Load headroom | 0.20 | Avoid overloaded clusters |
| Latency / proximity | 0.10 | Cross-region penalty |

### Scoring Formula

```
score(cluster) =
    cache_hit(prefix_hash, cluster) * 0.50
  + session_pinned(session_id, cluster) * 0.20
  + (1 - active_requests / capacity) * 0.20
  + max(0, (200 - avg_latency_ms) / 200) * 0.10
```

### Example

Request: chat continuation, session=S1, prefix_hash=H1

| Cluster | Cache | Session | Load (%) | Latency | Score |
|---------|-------|---------|----------|---------|-------|
| A | H1 match | S1 pinned | 60% | 15ms | 0.50 + 0.20 + 0.08 + 0.09 = **0.87** |
| B | no match | not pinned | 30% | 20ms | 0.00 + 0.00 + 0.14 + 0.09 = **0.23** |
| C | no match | not pinned | 80% | 50ms | 0.00 + 0.00 + 0.04 + 0.08 = **0.12** |

Winner: Cluster A (score 0.87).

## KV Cache Registry

### Data Structure (Redis)

```
Key: PREFIX:<hash>
Type: Set
Members: cluster_id values
TTL: 60 seconds
```

### Update Flow

```
1. vLLM pod generates tokens, populates PagedAttention cache
2. Every 5s, Cluster Agent scrapes vLLM /metrics
   - Extracts active prefix hashes from cache
3. Agent sends POST /internal/cache-report to Smart Router
   - Body: { cluster_id, prefix_hashes: [h1, h2, ...] }
4. Smart Router does SADD PREFIX:<hash> <cluster_id>
   - Sets TTL = 60s
5. Next request with matching prefix → cache_hit = true
```

### Eviction

- TTL-based: 60s default. If agent stops reporting a hash, it expires.
- Explicit: agent sends removal when pod evicts from cache.

## Session Router

### Data Structure (Redis)

```
Key: SESSION:<session_id>
Type: String
Value: cluster_id
TTL: 30 minutes
```

### When Sessions Are Pinned

- After first routing decision for a new session
- Smart Router calls SET SESSION:<id> <cluster_id> EX 1800

### When Sessions Are Released

- TTL expiry (30 min of inactivity)
- Explicit release when client sends session-end signal
- Force release when pinned cluster goes offline

## Semantic Router (Phase 4)

### Purpose

Route based on prompt intent to select the best MODEL CLASS,
not just the best cluster.

### Example Rules

| Intent | Model Class | Target |
|--------|-------------|--------|
| Code generation | Code model | clusters with CodeLlama |
| Medical Q&A | Domain model | clusters with Med-PaLM |
| Simple chat | Small model | clusters with 7B model |
| Complex reasoning | Large model | clusters with 70B+ |

### Implementation

- Lightweight ONNX classifier (~10MB) loaded in-memory in the router
- Classifies prompt into intent categories
- Intent maps to model class
- Model class filters cluster candidates before scoring

This is additive — it narrows the candidate set, then the normal
scoring algorithm picks the best cluster from the filtered set.

## Failure Modes

### Redis down
- Router falls back to round-robin across healthy clusters
- Session affinity and cache routing are temporarily disabled
- No data loss (Redis is a cache, not source of truth)

### All clusters full
- Router returns 503 with Retry-After header
- Queue depth alert fires
- Autoscaler should be adding capacity

### Cache miss on all clusters
- Normal — first request for a new prompt
- Router picks cluster with lowest load
- After first inference, cache is populated and reported
```
