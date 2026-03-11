package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

// --- Domain ---

type Cluster struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Region         string    `json:"region"`
	Provider       string    `json:"provider"` // internal, byoc, spot
	Endpoint       string    `json:"endpoint"`
	GPUType        string    `json:"gpu_type"`
	TotalGPUs      int       `json:"total_gpus"`
	AvailableGPUs  int       `json:"available_gpus"`
	DeployedModels []string  `json:"deployed_models"`
	Status         string    `json:"status"` // pending, healthy, degraded, offline
	LastHeartbeat  time.Time `json:"last_heartbeat"`
	AgentVersion   string    `json:"agent_version"`
	KubeVersion    string    `json:"kube_version"`
	Labels         map[string]string `json:"labels"`
	RegisteredAt   time.Time `json:"registered_at"`
}

type RegisterRequest struct {
	Name        string            `json:"name"`
	Region      string            `json:"region"`
	Provider    string            `json:"provider"`
	Endpoint    string            `json:"endpoint"`
	GPUType     string            `json:"gpu_type"`
	TotalGPUs   int               `json:"total_gpus"`
	AgentVersion string           `json:"agent_version"`
	KubeVersion string            `json:"kube_version"`
	Labels      map[string]string `json:"labels"`
}

type HeartbeatRequest struct {
	ClusterID      string   `json:"cluster_id"`
	AvailableGPUs  int      `json:"available_gpus"`
	ActiveRequests int      `json:"active_requests"`
	Capacity       int      `json:"capacity"`
	AvgLatencyMs   float64  `json:"avg_latency_ms"`
	DeployedModels []string `json:"deployed_models"`
	GPUUtilization float64  `json:"gpu_utilization"`
}

// --- Store ---

// In production, replace with DynamoDB. This in-memory store is for initial development.
type ClusterStore struct {
	clusters map[string]*Cluster
	mu       sync.RWMutex
}

func NewClusterStore() *ClusterStore {
	return &ClusterStore{
		clusters: make(map[string]*Cluster),
	}
}

func (s *ClusterStore) Register(c *Cluster) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusters[c.ID] = c
}

func (s *ClusterStore) Get(id string) (*Cluster, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clusters[id]
	return c, ok
}

func (s *ClusterStore) List() []*Cluster {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Cluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		out = append(out, c)
	}
	return out
}

func (s *ClusterStore) UpdateHeartbeat(id string, hb HeartbeatRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return false
	}
	c.AvailableGPUs = hb.AvailableGPUs
	c.DeployedModels = hb.DeployedModels
	c.LastHeartbeat = time.Now()
	c.Status = "healthy"
	return true
}

func (s *ClusterStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clusters, id)
}

// --- Health Monitor ---

func (s *ClusterStore) MonitorHealth(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			for _, c := range s.clusters {
				age := time.Since(c.LastHeartbeat)
				switch {
				case age > 60*time.Second:
					c.Status = "offline"
				case age > 30*time.Second:
					c.Status = "degraded"
				}
			}
			s.mu.Unlock()
		}
	}
}

// --- Server ---

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	store := NewClusterStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.MonitorHealth(ctx)

	app := fiber.New()

	// Register a new cluster
	app.Post("/api/clusters/register", func(c *fiber.Ctx) error {
		var req RegisterRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}

		clusterID := fmt.Sprintf("%s-%s-%d", req.Provider, req.Region, time.Now().UnixMilli())

		cluster := &Cluster{
			ID:            clusterID,
			Name:          req.Name,
			Region:        req.Region,
			Provider:      req.Provider,
			Endpoint:      req.Endpoint,
			GPUType:       req.GPUType,
			TotalGPUs:     req.TotalGPUs,
			AvailableGPUs: req.TotalGPUs,
			Status:        "pending",
			AgentVersion:  req.AgentVersion,
			KubeVersion:   req.KubeVersion,
			Labels:        req.Labels,
			RegisteredAt:  time.Now(),
			LastHeartbeat: time.Now(),
		}

		store.Register(cluster)

		log.Printf("Cluster registered: %s (%s, %s, %dx %s)",
			clusterID, req.Name, req.Region, req.TotalGPUs, req.GPUType)

		return c.Status(201).JSON(fiber.Map{
			"cluster_id": clusterID,
			"status":     "registered",
		})
	})

	// Heartbeat from cluster agent
	app.Post("/api/clusters/heartbeat", func(c *fiber.Ctx) error {
		var req HeartbeatRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}
		if !store.UpdateHeartbeat(req.ClusterID, req) {
			return c.Status(404).JSON(fiber.Map{"error": "cluster not found"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// List all clusters
	app.Get("/api/clusters", func(c *fiber.Ctx) error {
		return c.JSON(store.List())
	})

	// Get a single cluster
	app.Get("/api/clusters/:id", func(c *fiber.Ctx) error {
		cl, ok := store.Get(c.Params("id"))
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		return c.JSON(cl)
	})

	// Deregister a cluster
	app.Delete("/api/clusters/:id", func(c *fiber.Ctx) error {
		store.Delete(c.Params("id"))
		return c.JSON(fiber.Map{"status": "deleted"})
	})

	log.Printf("Cluster Manager starting on :%s", port)
	log.Fatal(app.Listen(":" + port))
}
