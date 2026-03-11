package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

// Function Manager
//
// A "Function" is a reusable template: define once, deploy many times.
// It stores the model URI, runtime config, resource requirements, and
// scaling policies. You can then create multiple "Instances" of a
// function across different clusters, tenants, or regions.
//
// Function  → the blueprint (what to run)
// Instance  → a running deployment of that function (where it runs)

// --- Domain ---

type Function struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Version      string            `json:"version"`
	Runtime      string            `json:"runtime"`
	ModelURI     string            `json:"model_uri"`
	ModelFormat  string            `json:"model_format"`
	Quantization string            `json:"quantization"`
	Resources    ResourceSpec      `json:"resources"`
	Scaling      ScalingSpec       `json:"scaling"`
	Routing      RoutingSpec       `json:"routing"`
	ExtraArgs    []string          `json:"extra_args"`
	EnvVars      map[string]string `json:"env_vars"`
	Tags         map[string]string `json:"tags"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	CreatedBy    string            `json:"created_by"`
}

type ResourceSpec struct {
	GPUType        string `json:"gpu_type"`
	GPUCount       int    `json:"gpu_count"`
	TensorParallel int    `json:"tensor_parallel"`
	MaxModelLen    int    `json:"max_model_len"`
	CPU            string `json:"cpu"`
	Memory         string `json:"memory"`
}

type ScalingSpec struct {
	MinReplicas int    `json:"min_replicas"`
	MaxReplicas int    `json:"max_replicas"`
	Metric      string `json:"metric"`
	TargetValue int    `json:"target_value"`
}

type RoutingSpec struct {
	PrefixCaching   bool `json:"prefix_caching"`
	SessionAffinity bool `json:"session_affinity"`
}

type Instance struct {
	ID           string    `json:"id"`
	FunctionID   string    `json:"function_id"`
	FunctionName string    `json:"function_name"`
	TenantID     string    `json:"tenant_id"`
	Cluster      string    `json:"cluster"`
	Region       string    `json:"region"`
	Cloud        string    `json:"cloud"`
	UseSpot      bool      `json:"use_spot"`
	Status       string    `json:"status"`
	Endpoint     string    `json:"endpoint"`
	Replicas     int       `json:"replicas"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// --- Store ---

type Store struct {
	functions map[string]*Function
	instances map[string]*Instance
	mu        sync.RWMutex
}

func NewStore() *Store {
	return &Store{
		functions: make(map[string]*Function),
		instances: make(map[string]*Instance),
	}
}

func (s *Store) CreateFunction(f *Function)                       { s.mu.Lock(); defer s.mu.Unlock(); s.functions[f.ID] = f }
func (s *Store) GetFunction(id string) (*Function, bool)          { s.mu.RLock(); defer s.mu.RUnlock(); f, ok := s.functions[id]; return f, ok }
func (s *Store) DeleteFunction(id string)                         { s.mu.Lock(); defer s.mu.Unlock(); delete(s.functions, id) }
func (s *Store) CreateInstance(inst *Instance)                     { s.mu.Lock(); defer s.mu.Unlock(); s.instances[inst.ID] = inst }
func (s *Store) GetInstance(id string) (*Instance, bool)           { s.mu.RLock(); defer s.mu.RUnlock(); i, ok := s.instances[id]; return i, ok }
func (s *Store) DeleteInstance(id string)                          { s.mu.Lock(); defer s.mu.Unlock(); delete(s.instances, id) }

func (s *Store) ListFunctions() []*Function {
	s.mu.RLock(); defer s.mu.RUnlock()
	out := make([]*Function, 0, len(s.functions))
	for _, f := range s.functions { out = append(out, f) }
	return out
}

func (s *Store) ListInstances(functionID string) []*Instance {
	s.mu.RLock(); defer s.mu.RUnlock()
	var out []*Instance
	for _, inst := range s.instances {
		if functionID == "" || inst.FunctionID == functionID {
			out = append(out, inst)
		}
	}
	return out
}

func (s *Store) UpdateFunction(id string, update func(*Function)) bool {
	s.mu.Lock(); defer s.mu.Unlock()
	f, ok := s.functions[id]
	if !ok { return false }
	update(f)
	f.UpdatedAt = time.Now()
	return true
}

// --- Server ---

func main() {
	port := envOr("PORT", "8086")
	configStoreURL := envOr("CONFIG_STORE_URL", "http://config-store:8087")

	store := NewStore()
	httpClient := &http.Client{Timeout: 10 * time.Second}

	app := fiber.New()
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Content-Type, Authorization",
	}))

	// ============ FUNCTIONS (blueprints) ============

	app.Post("/api/functions", func(c *fiber.Ctx) error {
		var f Function
		if err := c.BodyParser(&f); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}

		f.ID = fmt.Sprintf("fn-%d", time.Now().UnixMilli())
		f.CreatedAt = time.Now()
		f.UpdatedAt = time.Now()
		if f.Version == "" { f.Version = "v1" }
		if f.Resources.TensorParallel == 0 { f.Resources.TensorParallel = f.Resources.GPUCount }
		if f.Scaling.MinReplicas == 0 { f.Scaling.MinReplicas = 1 }
		if f.Scaling.MaxReplicas == 0 { f.Scaling.MaxReplicas = 10 }
		if f.Scaling.Metric == "" { f.Scaling.Metric = "queue_depth" }
		if f.Scaling.TargetValue == 0 { f.Scaling.TargetValue = 10 }

		store.CreateFunction(&f)

		go saveToConfigStore(httpClient, configStoreURL, &f)

		log.Printf("Function created: %s (%s, runtime=%s)", f.ID, f.Name, f.Runtime)
		return c.Status(201).JSON(f)
	})

	app.Get("/api/functions", func(c *fiber.Ctx) error {
		return c.JSON(store.ListFunctions())
	})

	app.Get("/api/functions/:id", func(c *fiber.Ctx) error {
		f, ok := store.GetFunction(c.Params("id"))
		if !ok { return c.Status(404).JSON(fiber.Map{"error": "not found"}) }
		return c.JSON(f)
	})

	app.Put("/api/functions/:id", func(c *fiber.Ctx) error {
		var update Function
		if err := c.BodyParser(&update); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}
		ok := store.UpdateFunction(c.Params("id"), func(f *Function) {
			if update.Name != "" { f.Name = update.Name }
			if update.Description != "" { f.Description = update.Description }
			if update.ModelURI != "" { f.ModelURI = update.ModelURI }
			if update.Runtime != "" { f.Runtime = update.Runtime }
			if update.Version != "" { f.Version = update.Version }
			if update.Resources.GPUCount > 0 { f.Resources = update.Resources }
			if update.Scaling.MinReplicas > 0 { f.Scaling = update.Scaling }
		})
		if !ok { return c.Status(404).JSON(fiber.Map{"error": "not found"}) }
		f, _ := store.GetFunction(c.Params("id"))
		go saveToConfigStore(httpClient, configStoreURL, f)
		return c.JSON(f)
	})

	app.Delete("/api/functions/:id", func(c *fiber.Ctx) error {
		store.DeleteFunction(c.Params("id"))
		return c.JSON(fiber.Map{"status": "deleted"})
	})

	// ============ INSTANCES (deploy a function) ============

	app.Post("/api/functions/:id/deploy", func(c *fiber.Ctx) error {
		f, ok := store.GetFunction(c.Params("id"))
		if !ok { return c.Status(404).JSON(fiber.Map{"error": "function not found"}) }

		var req struct {
			TenantID string `json:"tenant_id"`
			Cluster  string `json:"cluster"`
			Region   string `json:"region"`
			Cloud    string `json:"cloud"`
			UseSpot  bool   `json:"use_spot"`
		}
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
		}

		inst := &Instance{
			ID:           fmt.Sprintf("inst-%d", time.Now().UnixMilli()),
			FunctionID:   f.ID,
			FunctionName: f.Name,
			TenantID:     req.TenantID,
			Cluster:      req.Cluster,
			Region:       req.Region,
			Cloud:        req.Cloud,
			UseSpot:      req.UseSpot,
			Status:       "deploying",
			Replicas:     f.Scaling.MinReplicas,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		store.CreateInstance(inst)

		log.Printf("Instance %s deploying function %s (tenant=%s, cluster=%s)",
			inst.ID, f.Name, req.TenantID, req.Cluster)

		return c.Status(201).JSON(inst)
	})

	app.Get("/api/instances", func(c *fiber.Ctx) error {
		return c.JSON(store.ListInstances(c.Query("function_id")))
	})

	app.Get("/api/instances/:id", func(c *fiber.Ctx) error {
		inst, ok := store.GetInstance(c.Params("id"))
		if !ok { return c.Status(404).JSON(fiber.Map{"error": "not found"}) }
		return c.JSON(inst)
	})

	app.Delete("/api/instances/:id", func(c *fiber.Ctx) error {
		store.DeleteInstance(c.Params("id"))
		return c.JSON(fiber.Map{"status": "deleted"})
	})

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	log.Printf("Function Manager starting on :%s", port)
	log.Fatal(app.Listen(":" + port))
}

func saveToConfigStore(client *http.Client, baseURL string, f *Function) {
	data, _ := json.Marshal(f)
	resp, err := client.Post(baseURL+"/api/configs/"+f.ID, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("Config store save failed for %s: %v", f.ID, err)
		return
	}
	resp.Body.Close()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}
