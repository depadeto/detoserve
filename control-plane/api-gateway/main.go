// DetoServe API Gateway — single consumer-facing endpoint
//
// Consumers hit ONE endpoint:
//   POST http://gateway:8090/v1/chat/completions  (body: {"model": "llama-70b-chat", ...})
//   GET  http://gateway:8090/v1/models
//
// The gateway:
//  1. Reads the "model" field from the request
//  2. Looks up all running instances of that model from the Function Manager
//  3. Load-balances across them (round-robin, with tenant-scoped routing)
//  4. Proxies the request to the selected backend endpoint

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

const (
	defaultPort              = "8090"
	defaultFunctionManagerURL = "http://localhost:8086"
	refreshInterval          = 5 * time.Second
)

// --- Function Manager API types ---

type fmFunction struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Resources fmResources  `json:"resources"`
}

type fmResources struct {
	GPUType  string `json:"gpu_type"`
	GPUCount int    `json:"gpu_count"`
}

type fmInstance struct {
	ID             string `json:"id"`
	FunctionID     string `json:"function_id"`
	FunctionName   string `json:"function_name"`
	TenantID       string `json:"tenant_id"`
	Cluster        string `json:"cluster"`
	SkyClusterName string `json:"sky_cluster_name"`
	Status         string `json:"status"`
	Endpoint       string `json:"endpoint"`
}

// --- Backend registry ---

type backend struct {
	InstanceID string `json:"instance_id"`
	Endpoint   string `json:"endpoint"`
	TenantID   string `json:"tenant_id"`
	Cluster    string `json:"cluster"`
	SkyCluster string `json:"sky_cluster"`
	GPUType    string `json:"gpu_type"`
	GPUCount   int    `json:"gpu_count"`
}

type registry struct {
	mu           sync.RWMutex
	backends     map[string][]*backend
	roundRobin   map[string]*atomic.Uint64
	tenantRR     map[string]*atomic.Uint64
	lastRefresh  time.Time
	fmURL        string
	refreshInterval time.Duration
	httpClient   *http.Client
}

func newRegistry(fmURL string) *registry {
	return &registry{
		backends:       make(map[string][]*backend),
		roundRobin:     make(map[string]*atomic.Uint64),
		tenantRR:       make(map[string]*atomic.Uint64),
		fmURL:          fmURL,
		refreshInterval: refreshInterval,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
	}
}

func (r *registry) refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastRefresh) < r.refreshInterval {
		return
	}
	_ = r.doRefresh()
}

func (r *registry) doRefresh() error {
	fnResp, err := r.httpClient.Get(r.fmURL + "/api/functions")
	if err != nil {
		log.Printf("[gateway] Failed to fetch functions: %v", err)
		return err
	}
	defer fnResp.Body.Close()
	if fnResp.StatusCode != http.StatusOK {
		log.Printf("[gateway] Functions API returned %d", fnResp.StatusCode)
		return fmt.Errorf("functions API: %d", fnResp.StatusCode)
	}

	var functions []fmFunction
	if err := json.NewDecoder(fnResp.Body).Decode(&functions); err != nil {
		log.Printf("[gateway] Failed to parse functions: %v", err)
		return err
	}
	fnMap := make(map[string]*fmFunction)
	for i := range functions {
		fnMap[functions[i].ID] = &functions[i]
	}

	instResp, err := r.httpClient.Get(r.fmURL + "/api/instances")
	if err != nil {
		log.Printf("[gateway] Failed to fetch instances: %v", err)
		return err
	}
	defer instResp.Body.Close()
	if instResp.StatusCode != http.StatusOK {
		log.Printf("[gateway] Instances API returned %d", instResp.StatusCode)
		return fmt.Errorf("instances API: %d", instResp.StatusCode)
	}

	var instances []fmInstance
	if err := json.NewDecoder(instResp.Body).Decode(&instances); err != nil {
		log.Printf("[gateway] Failed to parse instances: %v", err)
		return err
	}

	newBackends := make(map[string][]*backend)
	for _, inst := range instances {
		if inst.Status != "running" || inst.Endpoint == "" {
			continue
		}
		fn := fnMap[inst.FunctionID]
		if fn == nil {
			continue
		}
		modelName := fn.Name
		if modelName == "" {
			modelName = inst.FunctionName
		}
		if modelName == "" {
			continue
		}
		b := &backend{
			InstanceID: inst.ID,
			Endpoint:   strings.TrimSuffix(inst.Endpoint, "/"),
			TenantID:   inst.TenantID,
			Cluster:    inst.Cluster,
			SkyCluster: inst.SkyClusterName,
			GPUType:    fn.Resources.GPUType,
			GPUCount:   fn.Resources.GPUCount,
		}
		newBackends[modelName] = append(newBackends[modelName], b)
	}

	r.backends = newBackends
	r.roundRobin = make(map[string]*atomic.Uint64)
	r.tenantRR = make(map[string]*atomic.Uint64)
	for name := range newBackends {
		r.roundRobin[name] = &atomic.Uint64{}
	}
	r.lastRefresh = time.Now()
	return nil
}

func (r *registry) selectBackend(model, tenantID string) *backend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	backends := r.backends[model]
	if len(backends) == 0 {
		return nil
	}

	if tenantID != "" {
		var tenantBackends []*backend
		for _, b := range backends {
			if b.TenantID == tenantID {
				tenantBackends = append(tenantBackends, b)
			}
		}
		if len(tenantBackends) > 0 {
			rrKey := model + ":" + tenantID
			if _, ok := r.tenantRR[rrKey]; !ok {
				r.tenantRR[rrKey] = &atomic.Uint64{}
			}
			idx := r.tenantRR[rrKey].Add(1) % uint64(len(tenantBackends))
			return tenantBackends[idx]
		}
	}

	rr := r.roundRobin[model]
	if rr == nil {
		return backends[0]
	}
	idx := rr.Add(1) % uint64(len(backends))
	return backends[idx]
}

func (r *registry) getModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	models := make([]string, 0, len(r.backends))
	for m := range r.backends {
		models = append(models, m)
	}
	return models
}

func (r *registry) getBackends() map[string][]*backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]*backend)
	for k, v := range r.backends {
		out[k] = append([]*backend(nil), v...)
	}
	return out
}

func (r *registry) ensureRefreshed() {
	r.mu.Lock()
	if time.Since(r.lastRefresh) >= r.refreshInterval {
		_ = r.doRefresh()
	}
	r.mu.Unlock()
}

// --- Proxy ---

var proxyClient = &http.Client{Timeout: 120 * time.Second}

func proxyRequest(method, targetURL string, body []byte, reqHeaders map[string]string) (int, []byte, map[string][]string, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, targetURL, bodyReader)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}

	resp, err := proxyClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, nil, err
	}

	// Copy response headers we care about
	headers := make(map[string][]string)
	for k, v := range resp.Header {
		headers[k] = v
	}
	return resp.StatusCode, respBody, headers, nil
}

// --- Main ---

func main() {
	port := envOr("GATEWAY_PORT", defaultPort)
	fmURL := envOr("FUNCTION_MANAGER_URL", defaultFunctionManagerURL)

	reg := newRegistry(fmURL)

	// Initial refresh
	reg.mu.Lock()
	_ = reg.doRefresh()
	reg.mu.Unlock()

	// Periodic refresh loop
	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			reg.refresh()
		}
	}()

	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 120 * time.Second,
	})
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "*",
		AllowHeaders: "*",
	}))

	// GET /v1/models — OpenAI-compatible model list
	app.Get("/v1/models", func(c *fiber.Ctx) error {
		reg.ensureRefreshed()
		backends := reg.getBackends()
		models := make([]fiber.Map, 0, len(backends))
		for name, bks := range backends {
			models = append(models, fiber.Map{
				"id":        name,
				"object":    "model",
				"owned_by":  "detoserve",
				"endpoints": len(bks),
			})
		}
		return c.JSON(fiber.Map{"object": "list", "data": models})
	})

	// POST /v1/chat/completions and POST /v1/completions — proxy inference
	proxyInference := func(c *fiber.Ctx) error {
		reg.ensureRefreshed()
		tenantID := c.Get("X-Tenant-ID")

		var body map[string]any
		if err := c.BodyParser(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid request body"})
		}
		model, _ := body["model"].(string)
		if model == "" {
			return c.Status(400).JSON(fiber.Map{"error": "missing 'model' field in request body"})
		}

		backend := reg.selectBackend(model, tenantID)
		if backend == nil {
			available := reg.getModels()
			return c.Status(404).JSON(fiber.Map{
				"error": fmt.Sprintf("no running instances for model '%s'. Available models: %v", model, available),
			})
		}

		targetURL := backend.Endpoint + "/v1/chat/completions"
		bodyBytes := c.Body()
		if len(bodyBytes) == 0 {
			var err error
			bodyBytes, err = json.Marshal(body)
			if err != nil {
				return c.Status(500).JSON(fiber.Map{"error": "failed to serialize body"})
			}
		}

		status, respBody, headers, err := proxyRequest(http.MethodPost, targetURL, bodyBytes, map[string]string{
			"X-DetoServe-Instance": backend.InstanceID,
			"X-DetoServe-Cluster":  backend.Cluster,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{
				"error": fmt.Sprintf("cannot reach backend %s (cluster: %s)", backend.Endpoint, backend.Cluster),
			})
		}

		c.Set("X-DetoServe-Instance", backend.InstanceID)
		c.Set("X-DetoServe-Cluster", backend.Cluster)
		c.Set("X-DetoServe-Backend", backend.Endpoint)
		for k, v := range headers {
			if len(v) > 0 && !strings.HasPrefix(strings.ToLower(k), "x-detoserve-") {
				c.Set(k, v[0])
			}
		}
		c.Status(status)
		c.Set("Content-Type", "application/json")
		return c.Send(respBody)
	}

	app.Post("/v1/chat/completions", proxyInference)
	app.Post("/v1/completions", proxyInference)

	// ALL /v1/* — catch-all proxy
	app.All("/v1/*", func(c *fiber.Ctx) error {
		reg.ensureRefreshed()
		tenantID := c.Get("X-Tenant-ID")
		bodyBytes := c.Body()

		var model string
		if c.Method() == "GET" {
			model = c.Query("model")
		} else if len(bodyBytes) > 0 {
			var body map[string]any
			_ = json.Unmarshal(bodyBytes, &body)
			if m, ok := body["model"].(string); ok {
				model = m
			}
		}

		if model == "" {
			backends := reg.getBackends()
			if len(backends) == 1 {
				for m := range backends {
					model = m
					break
				}
			}
		}
		if model == "" {
			return c.Status(400).JSON(fiber.Map{"error": "specify 'model' param or field"})
		}

		backend := reg.selectBackend(model, tenantID)
		if backend == nil {
			return c.Status(404).JSON(fiber.Map{
				"error": fmt.Sprintf("no instances for model '%s'", model),
			})
		}

		path := c.Params("*")
		targetURL := backend.Endpoint + "/v1/" + path
		method := c.Method()

		status, respBody, headers, err := proxyRequest(method, targetURL, bodyBytes, map[string]string{
			"X-DetoServe-Instance": backend.InstanceID,
			"X-DetoServe-Cluster":  backend.Cluster,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": fmt.Sprintf("proxy error: %v", err)})
		}

		c.Set("X-DetoServe-Instance", backend.InstanceID)
		c.Set("X-DetoServe-Cluster", backend.Cluster)
		c.Set("X-DetoServe-Backend", backend.Endpoint)
		for k, v := range headers {
			if len(v) > 0 && !strings.HasPrefix(strings.ToLower(k), "x-detoserve-") {
				c.Set(k, v[0])
			}
		}
		c.Status(status)
		c.Set("Content-Type", "application/json")
		return c.Send(respBody)
	})

	// GET /healthz — health check with routing table
	app.Get("/healthz", func(c *fiber.Ctx) error {
		reg.ensureRefreshed()
		backends := reg.getBackends()
		totalBackends := 0
		routingTable := make(map[string][]fiber.Map)
		for name, bks := range backends {
			totalBackends += len(bks)
			entries := make([]fiber.Map, 0, len(bks))
			for _, b := range bks {
				entries = append(entries, fiber.Map{
					"instance": b.InstanceID,
					"cluster":  b.Cluster,
					"endpoint": b.Endpoint,
				})
			}
			routingTable[name] = entries
		}
		return c.JSON(fiber.Map{
			"status":         "ok",
			"models":         len(backends),
			"total_backends": totalBackends,
			"routing_table":  routingTable,
		})
	})

	// GET / — root with usage instructions
	app.Get("/", func(c *fiber.Ctx) error {
		reg.ensureRefreshed()
		models := reg.getModels()
		return c.JSON(fiber.Map{
			"service": "DetoServe API Gateway",
			"version": "0.1.0",
			"usage": fiber.Map{
				"endpoint": fmt.Sprintf("http://localhost:%s/v1/chat/completions", port),
				"method":   "POST",
				"body": fiber.Map{
					"model":    "<model-name>",
					"messages": []fiber.Map{{"role": "user", "content": "Hello"}},
				},
				"headers": fiber.Map{"X-Tenant-ID": "(optional) your tenant ID"},
			},
			"available_models": models,
			"models_endpoint": fmt.Sprintf("http://localhost:%s/v1/models", port),
		})
	})

	log.Printf("DetoServe API Gateway starting on :%s", port)
	log.Printf("  Consumer endpoint: http://localhost:%s/v1/chat/completions", port)
	log.Printf("  Models:            http://localhost:%s/v1/models", port)
	log.Printf("  Function Manager:  %s", fmURL)
	log.Fatal(app.Listen(":" + port))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
