package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Deployment Manager — orchestrates model deployments via SkyPilot Bridge.
// Instead of generating raw K8s manifests, it calls the SkyPilot Bridge
// API which wraps `sky serve` operations.

// --- Domain ---

type ModelDeployment struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	TenantID        string            `json:"tenant_id"`
	Runtime         string            `json:"runtime"`
	ModelURI        string            `json:"model_uri"`
	GPUType         string            `json:"gpu_type"`
	GPUCount        int               `json:"gpu_count"`
	TensorParallel  int               `json:"tensor_parallel"`
	MaxModelLen     int               `json:"max_model_len"`
	MinReplicas     int               `json:"min_replicas"`
	MaxReplicas     int               `json:"max_replicas"`
	Cloud           string            `json:"cloud,omitempty"`
	Region          string            `json:"region,omitempty"`
	UseSpot         bool              `json:"use_spot"`
	ExtraArgs       []string          `json:"extra_args"`
	EnvVars         map[string]string `json:"env_vars"`
	Status          string            `json:"status"`
	SkyPilotService string            `json:"skypilot_service"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type DeployRequest struct {
	Name           string            `json:"name"`
	TenantID       string            `json:"tenant_id"`
	Runtime        string            `json:"runtime"`
	ModelURI       string            `json:"model_uri"`
	GPUType        string            `json:"gpu_type"`
	GPUCount       int               `json:"gpu_count"`
	TensorParallel int               `json:"tensor_parallel"`
	MaxModelLen    int               `json:"max_model_len"`
	MinReplicas    int               `json:"min_replicas"`
	MaxReplicas    int               `json:"max_replicas"`
	Cloud          string            `json:"cloud,omitempty"`
	Region         string            `json:"region,omitempty"`
	UseSpot        bool              `json:"use_spot"`
	ExtraArgs      []string          `json:"extra_args"`
	EnvVars        map[string]string `json:"env_vars"`
}

// --- Store ---

type DeploymentStore struct {
	deployments map[string]*ModelDeployment
	mu          sync.RWMutex
}

func NewDeploymentStore() *DeploymentStore {
	return &DeploymentStore{deployments: make(map[string]*ModelDeployment)}
}

func (s *DeploymentStore) Create(d *ModelDeployment)          { s.mu.Lock(); defer s.mu.Unlock(); s.deployments[d.ID] = d }
func (s *DeploymentStore) Get(id string) (*ModelDeployment, bool) { s.mu.RLock(); defer s.mu.RUnlock(); d, ok := s.deployments[id]; return d, ok }
func (s *DeploymentStore) Delete(id string)                   { s.mu.Lock(); defer s.mu.Unlock(); delete(s.deployments, id) }

func (s *DeploymentStore) ListByTenant(tenantID string) []*ModelDeployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*ModelDeployment
	for _, d := range s.deployments {
		if tenantID == "" || d.TenantID == tenantID {
			out = append(out, d)
		}
	}
	return out
}

// --- SkyPilot Bridge Client ---

type BridgeClient struct {
	baseURL string
	client  *http.Client
}

func NewBridgeClient(url string) *BridgeClient {
	return &BridgeClient{
		baseURL: url,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (b *BridgeClient) DeployService(req DeployRequest) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"name":            req.Name,
		"model_uri":       req.ModelURI,
		"runtime":         req.Runtime,
		"gpu_type":        req.GPUType,
		"gpu_count":       req.GPUCount,
		"tensor_parallel": req.TensorParallel,
		"max_model_len":   req.MaxModelLen,
		"min_replicas":    req.MinReplicas,
		"max_replicas":    req.MaxReplicas,
		"use_spot":        req.UseSpot,
		"extra_args":      req.ExtraArgs,
		"env_vars":        req.EnvVars,
	}
	if req.Cloud != "" {
		body["cloud"] = req.Cloud
	}
	if req.Region != "" {
		body["region"] = req.Region
	}
	return b.post("/api/services", body)
}

func (b *BridgeClient) ScaleService(name string, minR, maxR *int) (map[string]interface{}, error) {
	body := map[string]interface{}{}
	if minR != nil {
		body["min_replicas"] = *minR
	}
	if maxR != nil {
		body["max_replicas"] = *maxR
	}
	return b.patch(fmt.Sprintf("/api/services/%s/scale", name), body)
}

func (b *BridgeClient) DeleteService(name string) (map[string]interface{}, error) {
	return b.delete(fmt.Sprintf("/api/services/%s", name))
}

func (b *BridgeClient) GetServiceStatus(name string) (map[string]interface{}, error) {
	return b.get(fmt.Sprintf("/api/services/%s", name))
}

func (b *BridgeClient) ListServices() (map[string]interface{}, error) {
	return b.get("/api/services")
}

func (b *BridgeClient) post(path string, body interface{}) (map[string]interface{}, error) {
	data, _ := json.Marshal(body)
	resp, err := b.client.Post(b.baseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

func (b *BridgeClient) patch(path string, body interface{}) (map[string]interface{}, error) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", b.baseURL+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

func (b *BridgeClient) get(path string) (map[string]interface{}, error) {
	resp, err := b.client.Get(b.baseURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(raw, &result)
	return result, nil
}

func (b *BridgeClient) delete(path string) (map[string]interface{}, error) {
	req, _ := http.NewRequest("DELETE", b.baseURL+path, nil)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// --- Server ---

func main() {
	port := envOr("PORT", "8082")
	bridgeURL := envOr("SKYPILOT_BRIDGE_URL", "http://skypilot-bridge:8085")

	store := NewDeploymentStore()
	bridge := NewBridgeClient(bridgeURL)

	app := fiber.New()

	// Create deployment — calls SkyPilot Bridge to run `sky serve up`
	app.Post("/api/deployments", func(c *fiber.Ctx) error {
		var req DeployRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}

		// Defaults
		if req.Runtime == "" { req.Runtime = "vllm" }
		if req.GPUType == "" { req.GPUType = "A100" }
		if req.GPUCount == 0 { req.GPUCount = 4 }
		if req.TensorParallel == 0 { req.TensorParallel = req.GPUCount }
		if req.MinReplicas == 0 { req.MinReplicas = 1 }
		if req.MaxReplicas == 0 { req.MaxReplicas = 10 }
		if req.MaxModelLen == 0 { req.MaxModelLen = 8192 }

		deployID := fmt.Sprintf("deploy-%d", time.Now().UnixMilli())
		serviceName := fmt.Sprintf("%s-%s", req.TenantID, req.Name)

		deployment := &ModelDeployment{
			ID:              deployID,
			Name:            req.Name,
			TenantID:        req.TenantID,
			Runtime:         req.Runtime,
			ModelURI:        req.ModelURI,
			GPUType:         req.GPUType,
			GPUCount:        req.GPUCount,
			TensorParallel:  req.TensorParallel,
			MaxModelLen:     req.MaxModelLen,
			MinReplicas:     req.MinReplicas,
			MaxReplicas:     req.MaxReplicas,
			Cloud:           req.Cloud,
			Region:          req.Region,
			UseSpot:         req.UseSpot,
			ExtraArgs:       req.ExtraArgs,
			EnvVars:         req.EnvVars,
			Status:          "deploying",
			SkyPilotService: serviceName,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		}
		store.Create(deployment)

		// Call SkyPilot Bridge asynchronously
		go func() {
			result, err := bridge.DeployService(req)
			if err != nil {
				log.Printf("SkyPilot deploy failed for %s: %v", deployID, err)
				deployment.Status = "failed"
			} else if status, ok := result["status"].(string); ok && status == "error" {
				log.Printf("SkyPilot deploy error for %s: %v", deployID, result["error"])
				deployment.Status = "failed"
			} else {
				deployment.Status = "running"
			}
			deployment.UpdatedAt = time.Now()
		}()

		log.Printf("Deployment %s created: %s/%s (runtime=%s, gpus=%dx%s)",
			deployID, req.TenantID, req.Name, req.Runtime, req.GPUCount, req.GPUType)

		return c.Status(201).JSON(fiber.Map{
			"deployment_id":   deployID,
			"skypilot_service": serviceName,
			"status":          "deploying",
		})
	})

	// List deployments
	app.Get("/api/deployments", func(c *fiber.Ctx) error {
		tenantID := c.Query("tenant_id")
		return c.JSON(store.ListByTenant(tenantID))
	})

	// Get deployment
	app.Get("/api/deployments/:id", func(c *fiber.Ctx) error {
		d, ok := store.Get(c.Params("id"))
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		return c.JSON(d)
	})

	// Scale deployment
	app.Patch("/api/deployments/:id/scale", func(c *fiber.Ctx) error {
		d, ok := store.Get(c.Params("id"))
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}

		var body struct {
			MinReplicas *int `json:"min_replicas"`
			MaxReplicas *int `json:"max_replicas"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}

		result, err := bridge.ScaleService(d.SkyPilotService, body.MinReplicas, body.MaxReplicas)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "bridge unavailable"})
		}
		return c.JSON(result)
	})

	// Delete deployment
	app.Delete("/api/deployments/:id", func(c *fiber.Ctx) error {
		d, ok := store.Get(c.Params("id"))
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}

		result, err := bridge.DeleteService(d.SkyPilotService)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "bridge unavailable"})
		}
		store.Delete(d.ID)
		return c.JSON(result)
	})

	// Proxy to SkyPilot status
	app.Get("/api/skypilot/services", func(c *fiber.Ctx) error {
		result, err := bridge.ListServices()
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "bridge unavailable"})
		}
		return c.JSON(result)
	})

	log.Printf("Deployment Manager starting on :%s (bridge=%s)", port, bridgeURL)
	log.Fatal(app.Listen(":" + port))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
