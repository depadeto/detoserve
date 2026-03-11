package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

// --- Config ---

type Config struct {
	Port             string
	RedisAddr        string
	WeightCache      float64
	WeightSession    float64
	WeightLoad       float64
	WeightLatency    float64
	PrefixTokenCount int
}

func loadConfig() Config {
	return Config{
		Port:             envOr("PORT", "8080"),
		RedisAddr:        envOr("REDIS_ADDR", "redis-cluster:6379"),
		WeightCache:      0.50,
		WeightSession:    0.20,
		WeightLoad:       0.20,
		WeightLatency:    0.10,
		PrefixTokenCount: 128,
	}
}

// --- Domain ---

type ClusterInfo struct {
	ID             string
	Region         string
	Endpoint       string
	GPUType        string
	TotalGPUs      int
	AvailableGPUs  int
	ActiveRequests int
	Capacity       int
	AvgLatencyMs   float64
	DeployedModels []string
	LastHeartbeat  time.Time
	Healthy        bool
}

type RouteDecision struct {
	ClusterID string
	Score     float64
	Reason    string
}

// --- Smart Router ---

type SmartRouter struct {
	cfg            Config
	redis          *redis.Client
	clusters       map[string]*ClusterInfo
	mu             sync.RWMutex
}

func NewSmartRouter(cfg Config) *SmartRouter {
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
	})
	return &SmartRouter{
		cfg:      cfg,
		redis:    rdb,
		clusters: make(map[string]*ClusterInfo),
	}
}

// Route computes the best cluster for a request.
func (r *SmartRouter) Route(ctx context.Context, req RoutingRequest) (*RouteDecision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	candidates := r.filterCandidates(req)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no healthy cluster available for model %s", req.Model)
	}

	type scored struct {
		cluster *ClusterInfo
		score   float64
		reason  string
	}
	results := make([]scored, 0, len(candidates))

	for _, c := range candidates {
		s, reason := r.scoreCluster(ctx, c, req)
		results = append(results, scored{cluster: c, score: s, reason: reason})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	best := results[0]
	decision := &RouteDecision{
		ClusterID: best.cluster.ID,
		Score:     best.score,
		Reason:    best.reason,
	}

	// Pin session for future requests
	if req.SessionID != "" {
		r.pinSession(ctx, req.SessionID, best.cluster.ID)
	}

	return decision, nil
}

func (r *SmartRouter) filterCandidates(req RoutingRequest) []*ClusterInfo {
	var out []*ClusterInfo
	for _, c := range r.clusters {
		if !c.Healthy {
			continue
		}
		if c.ActiveRequests >= c.Capacity {
			continue
		}
		if req.Model != "" && !contains(c.DeployedModels, req.Model) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (r *SmartRouter) scoreCluster(ctx context.Context, c *ClusterInfo, req RoutingRequest) (float64, string) {
	score := 0.0
	reasons := ""

	// Signal 1: KV-cache locality
	if req.PrefixHash != "" {
		cached, _ := r.redis.SIsMember(ctx, "PREFIX:"+req.PrefixHash, c.ID).Result()
		if cached {
			score += r.cfg.WeightCache
			reasons += "cache_hit "
		}
	}

	// Signal 2: Session affinity
	if req.SessionID != "" {
		pinned, _ := r.redis.Get(ctx, "SESSION:"+req.SessionID).Result()
		if pinned == c.ID {
			score += r.cfg.WeightSession
			reasons += "session_pinned "
		}
	}

	// Signal 3: Load headroom
	if c.Capacity > 0 {
		loadRatio := float64(c.ActiveRequests) / float64(c.Capacity)
		score += (1 - loadRatio) * r.cfg.WeightLoad
		reasons += fmt.Sprintf("load=%.0f%% ", loadRatio*100)
	}

	// Signal 4: Latency
	maxLatency := 200.0
	latencyScore := math.Max(0, (maxLatency-c.AvgLatencyMs)/maxLatency)
	score += latencyScore * r.cfg.WeightLatency
	reasons += fmt.Sprintf("lat=%.0fms", c.AvgLatencyMs)

	return score, reasons
}

func (r *SmartRouter) pinSession(ctx context.Context, sessionID, clusterID string) {
	r.redis.Set(ctx, "SESSION:"+sessionID, clusterID, 30*time.Minute)
}

// UpdateCluster is called by cluster agents via heartbeat.
func (r *SmartRouter) UpdateCluster(info *ClusterInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info.LastHeartbeat = time.Now()
	info.Healthy = true
	r.clusters[info.ID] = info
}

// MarkUnhealthy detects clusters that missed heartbeats.
func (r *SmartRouter) HealthCheck() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clusters {
		if time.Since(c.LastHeartbeat) > 30*time.Second {
			c.Healthy = false
		}
	}
}

// --- HTTP Types ---

type RoutingRequest struct {
	Model      string
	SessionID  string
	TenantID   string
	PrefixHash string
	Prompt     string
}

// ComputePrefixHash hashes the first N characters of the prompt.
func ComputePrefixHash(prompt string, n int) string {
	if len(prompt) > n {
		prompt = prompt[:n]
	}
	h := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(h[:8])
}

// --- HTTP Server ---

func main() {
	cfg := loadConfig()
	router := NewSmartRouter(cfg)

	// Health check loop
	go func() {
		for {
			time.Sleep(10 * time.Second)
			router.HealthCheck()
		}
	}()

	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 120 * time.Second,
	})

	// OpenAI-compatible inference proxy
	app.All("/v1/*", func(c *fiber.Ctx) error {
		ctx := c.Context()

		req := RoutingRequest{
			Model:     c.Query("model", c.Get("X-Model")),
			SessionID: c.Get("X-Session-ID"),
			TenantID:  c.Get("X-Tenant-ID"),
		}

		// For chat completions, extract prefix for cache routing
		if c.Path() == "/v1/chat/completions" && len(c.Body()) > 0 {
			req.PrefixHash = ComputePrefixHash(string(c.Body()), cfg.PrefixTokenCount)
		}

		decision, err := router.Route(ctx, req)
		if err != nil {
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{
				"error": err.Error(),
			})
		}

		cluster := router.clusters[decision.ClusterID]
		targetURL := fmt.Sprintf("%s%s", cluster.Endpoint, c.OriginalURL())

		// TODO: proxy the request to targetURL with streaming support
		// For now return the routing decision for debugging
		return c.JSON(fiber.Map{
			"routed_to": decision.ClusterID,
			"score":     decision.Score,
			"reason":    decision.Reason,
			"target":    targetURL,
		})
	})

	// Cluster heartbeat endpoint
	app.Post("/internal/heartbeat", func(c *fiber.Ctx) error {
		var info ClusterInfo
		if err := c.BodyParser(&info); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		router.UpdateCluster(&info)
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// KV cache prefix update
	app.Post("/internal/cache-report", func(c *fiber.Ctx) error {
		var report struct {
			ClusterID    string   `json:"cluster_id"`
			PrefixHashes []string `json:"prefix_hashes"`
		}
		if err := c.BodyParser(&report); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}

		ctx := c.Context()
		pipe := router.redis.Pipeline()
		for _, h := range report.PrefixHashes {
			pipe.SAdd(ctx, "PREFIX:"+h, report.ClusterID)
			pipe.Expire(ctx, "PREFIX:"+h, 60*time.Second)
		}
		pipe.Exec(ctx)

		return c.JSON(fiber.Map{"accepted": len(report.PrefixHashes)})
	})

	// Router stats
	app.Get("/internal/stats", func(c *fiber.Ctx) error {
		router.mu.RLock()
		defer router.mu.RUnlock()

		stats := make([]fiber.Map, 0)
		for _, cl := range router.clusters {
			stats = append(stats, fiber.Map{
				"id":              cl.ID,
				"region":          cl.Region,
				"healthy":         cl.Healthy,
				"gpu_available":   cl.AvailableGPUs,
				"active_requests": cl.ActiveRequests,
				"avg_latency_ms":  cl.AvgLatencyMs,
				"models":          cl.DeployedModels,
			})
		}
		return c.JSON(stats)
	})

	log.Printf("Smart Router starting on :%s", cfg.Port)
	log.Fatal(app.Listen(":" + cfg.Port))
}

// --- Helpers ---

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
