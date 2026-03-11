# NVIDIA Dynamo / LLM-D Integration

## Overview

NVIDIA Dynamo (and its open-source predecessor LLM-D) implements
disaggregated prefill/decode architecture for LLM serving. Instead of
running prefill and decode on the same GPU, they are split across pods:

```
Prefill Pod (GPU A)
    → processes prompt, generates KV cache
    → transfers KV cache to Decode Pod

Decode Pod (GPU B)
    → receives KV cache
    → generates tokens autoregressively
```

This improves GPU utilization because:
- Prefill is compute-bound (high FLOPS)
- Decode is memory-bound (low FLOPS, high bandwidth)

## Architecture in Our Platform

```
Smart Router
    │
    ▼
Cluster Agent
    │
    ▼
Dynamo Router (per cluster)
    │
    ├─ Prefill Pods (compute-optimized)
    │      └─ GPU: high FLOPS
    │
    └─ Decode Pods (memory-optimized)
           └─ GPU: high bandwidth
    │
    ▼
KV Transfer Layer (NCCL / RDMA)
```

## ModelDeployment CRD for Dynamo

```yaml
apiVersion: inference.detoserve.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: llama-70b-dynamo
spec:
  model:
    uri: hf://meta-llama/Llama-3.1-70B-Instruct
    quantization: fp8
  runtime:
    engine: dynamo
    tensorParallel: 4
    pipelineParallel: 2
    extraArgs:
      - "--disaggregated-prefill"
      - "--kv-transfer-method=nccl"
      - "--prefill-tp=4"
      - "--decode-tp=2"
  resources:
    gpuCount: 8     # 4 prefill + 4 decode (2 pods x 2 TP)
```

## How Dynamo Interacts With Smart Router

The Smart Router routes at the CLUSTER level. Inside the cluster:

1. Request arrives at Dynamo Router
2. Dynamo Router assigns to a Prefill pod
3. Prefill pod processes prompt, generates KV cache
4. KV cache is transferred to a Decode pod via NCCL
5. Decode pod generates tokens
6. Response streams back

The Smart Router's KV Cache Registry tracks DECODE pod locations
(since that's where the reusable cache lives).

## Autoscaling Dynamo

Prefill and Decode pods scale independently:

| Pod Type | Scale Metric | Behavior |
|----------|-------------|----------|
| Prefill | Prefill queue depth | Scale up when prompts are waiting |
| Decode | Active sequences | Scale up when many concurrent generations |

## When to Use Dynamo vs vLLM

| Scenario | Recommendation |
|----------|---------------|
| Low latency, < 32 concurrent users | vLLM (simpler) |
| High throughput, many concurrent users | Dynamo (better GPU util) |
| Very long prompts (> 4K tokens) | Dynamo (prefill offload) |
| Small models (< 13B) | vLLM (overhead not worth it) |
| Large models (70B+) | Dynamo (disaggregation helps) |
