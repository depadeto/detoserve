package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

// --- Domain ---

type Tenant struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	APIKeyHash      string   `json:"-"`
	APIKeyPrefix    string   `json:"api_key_prefix"` // first 8 chars for display
	AllowedModels   []string `json:"allowed_models"`
	AllowedClusters []string `json:"allowed_clusters"`
	GPUQuota        int      `json:"gpu_quota"`
	GPUUsed         int      `json:"gpu_used"`
	RateLimitRPS    int      `json:"rate_limit_rps"`
	Namespace       string   `json:"namespace"`
	Status          string   `json:"status"` // active, suspended, pending
	CreatedAt       time.Time `json:"created_at"`
	Usage           TenantUsage `json:"usage"`
}

type TenantUsage struct {
	TotalRequests    int64   `json:"total_requests"`
	TotalTokensIn    int64   `json:"total_tokens_in"`
	TotalTokensOut   int64   `json:"total_tokens_out"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	CurrentMonthReqs int64   `json:"current_month_reqs"`
}

type CreateTenantRequest struct {
	Name            string   `json:"name"`
	AllowedModels   []string `json:"allowed_models"`
	AllowedClusters []string `json:"allowed_clusters"`
	GPUQuota        int      `json:"gpu_quota"`
	RateLimitRPS    int      `json:"rate_limit_rps"`
}

// --- Store ---

type TenantStore struct {
	tenants    map[string]*Tenant
	keyIndex   map[string]string // apiKeyHash → tenantID
	mu         sync.RWMutex
}

func NewTenantStore() *TenantStore {
	return &TenantStore{
		tenants:  make(map[string]*Tenant),
		keyIndex: make(map[string]string),
	}
}

func (s *TenantStore) Create(t *Tenant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants[t.ID] = t
	s.keyIndex[t.APIKeyHash] = t.ID
}

func (s *TenantStore) Get(id string) (*Tenant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tenants[id]
	return t, ok
}

func (s *TenantStore) GetByAPIKey(apiKey string) (*Tenant, bool) {
	hash := hashKey(apiKey)
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.keyIndex[hash]
	if !ok {
		return nil, false
	}
	t, ok := s.tenants[id]
	return t, ok
}

func (s *TenantStore) List() []*Tenant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		out = append(out, t)
	}
	return out
}

// --- Helpers ---

func generateAPIKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// --- Server ---

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8083"
	}

	store := NewTenantStore()
	app := fiber.New()

	// Create tenant
	app.Post("/api/tenants", func(c *fiber.Ctx) error {
		var req CreateTenantRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}

		if req.GPUQuota == 0 {
			req.GPUQuota = 4
		}
		if req.RateLimitRPS == 0 {
			req.RateLimitRPS = 100
		}

		apiKey := generateAPIKey()
		tenantID := fmt.Sprintf("tenant-%d", time.Now().UnixMilli())

		tenant := &Tenant{
			ID:              tenantID,
			Name:            req.Name,
			APIKeyHash:      hashKey(apiKey),
			APIKeyPrefix:    apiKey[:11],
			AllowedModels:   req.AllowedModels,
			AllowedClusters: req.AllowedClusters,
			GPUQuota:        req.GPUQuota,
			RateLimitRPS:    req.RateLimitRPS,
			Namespace:       fmt.Sprintf("ns-%s", tenantID),
			Status:          "active",
			CreatedAt:       time.Now(),
		}

		store.Create(tenant)

		log.Printf("Tenant created: %s (%s, quota=%d GPUs)", tenantID, req.Name, req.GPUQuota)

		return c.Status(201).JSON(fiber.Map{
			"tenant_id": tenantID,
			"api_key":   apiKey,
			"namespace": tenant.Namespace,
			"note":      "Save this API key — it will not be shown again.",
		})
	})

	// Authenticate request (called by gateway ext_proc)
	app.Post("/api/auth/verify", func(c *fiber.Ctx) error {
		var body struct {
			APIKey string `json:"api_key"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "missing api_key"})
		}

		tenant, ok := store.GetByAPIKey(body.APIKey)
		if !ok {
			return c.Status(401).JSON(fiber.Map{"error": "invalid api key"})
		}
		if tenant.Status != "active" {
			return c.Status(403).JSON(fiber.Map{"error": "tenant suspended"})
		}

		return c.JSON(fiber.Map{
			"tenant_id":       tenant.ID,
			"allowed_models":  tenant.AllowedModels,
			"rate_limit_rps":  tenant.RateLimitRPS,
			"gpu_quota":       tenant.GPUQuota,
			"gpu_used":        tenant.GPUUsed,
		})
	})

	// List tenants
	app.Get("/api/tenants", func(c *fiber.Ctx) error {
		return c.JSON(store.List())
	})

	// Get tenant
	app.Get("/api/tenants/:id", func(c *fiber.Ctx) error {
		t, ok := store.Get(c.Params("id"))
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		return c.JSON(t)
	})

	// Usage tracking (called after each inference request)
	app.Post("/api/tenants/:id/usage", func(c *fiber.Ctx) error {
		var body struct {
			TokensIn  int64   `json:"tokens_in"`
			TokensOut int64   `json:"tokens_out"`
			CostUSD   float64 `json:"cost_usd"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}

		store.mu.Lock()
		defer store.mu.Unlock()
		t, ok := store.tenants[c.Params("id")]
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		t.Usage.TotalRequests++
		t.Usage.TotalTokensIn += body.TokensIn
		t.Usage.TotalTokensOut += body.TokensOut
		t.Usage.TotalCostUSD += body.CostUSD
		t.Usage.CurrentMonthReqs++

		return c.JSON(fiber.Map{"status": "recorded"})
	})

	log.Printf("Tenant Manager starting on :%s", port)
	log.Fatal(app.Listen(":" + port))
}
