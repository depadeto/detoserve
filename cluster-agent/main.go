package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// Cache Reporter Sidecar
//
// Deploys alongside vLLM pods in each SkyPilot-managed cluster.
// Scrapes vLLM /metrics to discover active prefix cache entries,
// then reports them to the Smart Router so it can route requests
// to the cluster holding the cache.
//
// Also reports cluster-level load metrics (queue depth, GPU util)
// to enable the Smart Router's load-based scoring.

type Config struct {
	SmartRouterURL string
	ClusterID      string
	VLLMMetricsURL string
	ReportInterval time.Duration
	Port           string
}

func loadConfig() Config {
	interval := 5
	fmt.Sscanf(envOr("REPORT_INTERVAL_SEC", "5"), "%d", &interval)
	return Config{
		SmartRouterURL: envOr("SMART_ROUTER_URL", "http://smart-router:8080"),
		ClusterID:      envOr("CLUSTER_ID", "unknown"),
		VLLMMetricsURL: envOr("VLLM_METRICS_URL", "http://localhost:8000/metrics"),
		ReportInterval: time.Duration(interval) * time.Second,
		Port:           envOr("PORT", "9090"),
	}
}

type CacheReporter struct {
	cfg    Config
	client *http.Client
}

func NewCacheReporter(cfg Config) *CacheReporter {
	return &CacheReporter{
		cfg:    cfg,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// ReportLoop runs every N seconds: scrapes vLLM metrics, reports to Smart Router.
func (cr *CacheReporter) ReportLoop() {
	ticker := time.NewTicker(cr.cfg.ReportInterval)
	defer ticker.Stop()

	for range ticker.C {
		cr.report()
	}
}

func (cr *CacheReporter) report() {
	prefixHashes := cr.scrapePrefixHashes()
	loadMetrics := cr.scrapeLoadMetrics()

	// Report prefix hashes for cache-aware routing
	if len(prefixHashes) > 0 {
		body := map[string]interface{}{
			"cluster_id":    cr.cfg.ClusterID,
			"prefix_hashes": prefixHashes,
		}
		cr.post("/internal/cache-report", body)
	}

	// Report cluster load for load-based scoring
	heartbeat := map[string]interface{}{
		"id":              cr.cfg.ClusterID,
		"active_requests": loadMetrics.ActiveRequests,
		"capacity":        loadMetrics.Capacity,
		"avg_latency_ms":  loadMetrics.AvgLatencyMs,
		"healthy":         true,
	}
	cr.post("/internal/heartbeat", heartbeat)
}

type LoadMetrics struct {
	ActiveRequests int
	Capacity       int
	AvgLatencyMs   float64
	QueueDepth     int
	GPUUtilization float64
}

func (cr *CacheReporter) scrapePrefixHashes() []string {
	// In production: GET vLLM /metrics, parse Prometheus text format,
	// extract prefix cache entries. vLLM exposes:
	//   vllm:num_prefix_blocks
	//   vllm:prefix_cache_hit_rate
	//
	// For actual prefix hashes, we'd need to call vLLM's internal API
	// or inspect the PagedAttention cache state.
	//
	// Placeholder — replace with real scraping logic.
	return []string{}
}

func (cr *CacheReporter) scrapeLoadMetrics() LoadMetrics {
	// In production: scrape vLLM /metrics for:
	//   vllm:num_requests_running
	//   vllm:num_requests_waiting
	//   vllm:avg_generation_throughput_toks_per_s
	//   vllm:avg_prompt_throughput_toks_per_s
	//
	// And DCGM exporter for GPU utilization.
	return LoadMetrics{}
}

func (cr *CacheReporter) post(path string, body interface{}) {
	data, _ := json.Marshal(body)
	url := cr.cfg.SmartRouterURL + path
	resp, err := cr.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("Report to %s failed: %v", path, err)
		return
	}
	resp.Body.Close()
}

// --- Health endpoint ---

func main() {
	cfg := loadConfig()
	reporter := NewCacheReporter(cfg)

	go reporter.ReportLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"cluster_id":    cfg.ClusterID,
			"router_url":    cfg.SmartRouterURL,
			"vllm_metrics":  cfg.VLLMMetricsURL,
			"report_interval": cfg.ReportInterval.String(),
		})
	})

	log.Printf("Cache Reporter starting on :%s (cluster=%s)", cfg.Port, cfg.ClusterID)
	http.ListenAndServe(":"+cfg.Port, mux)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
