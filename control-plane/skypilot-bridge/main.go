// SkyPilot Bridge — Go Fiber wrapper around SkyPilot CLI.
//
// Translates REST API calls from the Go control plane services
// into SkyPilot CLI operations for cluster provisioning,
// service deployment, scaling, and status queries.
//
// Also serves as the aggregation point for cluster agent heartbeats,
// providing a unified /api/clusters endpoint for the frontend.

package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

const (
	staleThresholdSec = 60
	defaultPort       = "8085"
	defaultServicesDir = "/app/services"
)

// Cluster registry state
var (
	clusters     = make(map[string]map[string]interface{})
	clustersMu   sync.RWMutex
)

type skyResult struct {
	Returncode int
	Stdout     string
	Stderr     string
}

// Request models
type DeployServiceRequest struct {
	Name           string            `json:"name"`
	ModelURI       string            `json:"model_uri"`
	Runtime        string            `json:"runtime"`
	GPUType        string            `json:"gpu_type"`
	GPUCount       int               `json:"gpu_count"`
	TensorParallel int               `json:"tensor_parallel"`
	MaxModelLen    int               `json:"max_model_len"`
	MinReplicas    int               `json:"min_replicas"`
	MaxReplicas    int               `json:"max_replicas"`
	Cloud          *string           `json:"cloud"`
	Region         *string           `json:"region"`
	UseSpot        bool              `json:"use_spot"`
	ExtraArgs      []string          `json:"extra_args"`
	EnvVars        map[string]string `json:"env_vars"`
}

type ScaleServiceRequest struct {
	MinReplicas *int `json:"min_replicas"`
	MaxReplicas *int `json:"max_replicas"`
}

type LaunchClusterRequest struct {
	Name                   string  `json:"name"`
	GPUType                string  `json:"gpu_type"`
	GPUCount               int     `json:"gpu_count"`
	Cloud                  *string `json:"cloud"`
	Region                 *string `json:"region"`
	UseSpot                bool    `json:"use_spot"`
	IdleMinutesToAutostop  int     `json:"idle_minutes_to_autostop"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func runSkyCmd(args []string, timeoutSec int) (skyResult, error) {
	ctx := context.Background()
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "sky", args...)

	log.Printf("Running: sky %s", strings.Join(args, " "))

	out, err := cmd.CombinedOutput()
	stdout := string(out)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return skyResult{Returncode: 1, Stdout: "", Stderr: "sky CLI not found"}, nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return skyResult{}, err
		}
		if ee, ok := err.(*exec.ExitError); ok {
			stderr := string(ee.Stderr)
			if stderr == "" {
				stderr = stdout
			}
			return skyResult{Returncode: ee.ExitCode(), Stdout: stdout, Stderr: stderr}, nil
		}
		return skyResult{Returncode: 1, Stdout: stdout, Stderr: err.Error()}, nil
	}
	return skyResult{Returncode: 0, Stdout: stdout, Stderr: ""}, nil
}

func generateServiceYAML(req *DeployServiceRequest) (string, error) {
	var runCmd, setupCmd, readinessPath string
	var initialDelay int

	switch req.Runtime {
	case "":
		req.Runtime = "vllm"
		fallthrough
	case "vllm":
		runCmd = "python -m vllm.entrypoints.openai.api_server " +
			"--model " + req.ModelURI + " " +
			"--tensor-parallel-size " + strconv.Itoa(req.TensorParallel) + " " +
			"--max-model-len " + strconv.Itoa(req.MaxModelLen) + " " +
			"--enable-prefix-caching " +
			"--port 8000"
		if len(req.ExtraArgs) > 0 {
			runCmd += " " + strings.Join(req.ExtraArgs, " ")
		}
		setupCmd = "pip install vllm"
		readinessPath = "/health"
		initialDelay = 120
	case "triton":
		runCmd = "tritonserver --model-repository=/models --http-port=8000"
		setupCmd = ""
		readinessPath = "/v2/health/ready"
		initialDelay = 60
	case "dynamo":
		runCmd = "dynamo serve --model " + req.ModelURI + " " +
			"--disaggregated-prefill --port 8000"
		if len(req.ExtraArgs) > 0 {
			runCmd += " " + strings.Join(req.ExtraArgs, " ")
		}
		setupCmd = "pip install nvidia-dynamo"
		readinessPath = "/health"
		initialDelay = 180
	default:
		return "", nil // will be handled as 400
	}

	if req.GPUType == "" {
		req.GPUType = "A100"
	}
	if req.GPUCount == 0 {
		req.GPUCount = 4
	}
	if req.TensorParallel == 0 {
		req.TensorParallel = 4
	}
	if req.MaxModelLen == 0 {
		req.MaxModelLen = 8192
	}
	if req.MinReplicas == 0 {
		req.MinReplicas = 2
	}
	if req.MaxReplicas == 0 {
		req.MaxReplicas = 20
	}

	var sb strings.Builder
	sb.WriteString("service:\n")
	sb.WriteString("  replicas:\n")
	sb.WriteString("    min: " + strconv.Itoa(req.MinReplicas) + "\n")
	sb.WriteString("    max: " + strconv.Itoa(req.MaxReplicas) + "\n")
	sb.WriteString("  readiness_probe:\n")
	sb.WriteString("    path: " + readinessPath + "\n")
	sb.WriteString("    initial_delay_seconds: " + strconv.Itoa(initialDelay) + "\n")
	sb.WriteString("\nresources:\n")
	sb.WriteString("  accelerators: " + req.GPUType + ":" + strconv.Itoa(req.GPUCount) + "\n")
	if req.Cloud != nil && *req.Cloud != "" {
		sb.WriteString("  cloud: " + *req.Cloud + "\n")
	}
	if req.Region != nil && *req.Region != "" {
		sb.WriteString("  region: " + *req.Region + "\n")
	}
	if req.UseSpot {
		sb.WriteString("  use_spot: true\n")
	}
	sb.WriteString("\n")

	if len(req.EnvVars) > 0 {
		sb.WriteString("envs:\n")
		for k, v := range req.EnvVars {
			sb.WriteString("  " + k + ": " + v + "\n")
		}
		sb.WriteString("\n")
	}
	if setupCmd != "" {
		sb.WriteString("setup: |\n")
		sb.WriteString("  " + setupCmd + "\n\n")
	}
	sb.WriteString("run: |\n")
	sb.WriteString("  " + runCmd + "\n")

	return sb.String(), nil
}

func main() {
	port := envOr("PORT", defaultPort)
	servicesDir := envOr("SERVICES_DIR", defaultServicesDir)

	os.MkdirAll(servicesDir, 0755)

	app := fiber.New()
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PATCH,DELETE,OPTIONS",
		AllowHeaders: "*",
	}))

	// Health
	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// --- Cluster registry (heartbeats) ---
	// Must register heartbeat and provision before :cluster_id
	app.Post("/api/clusters/heartbeat", func(c *fiber.Ctx) error {
		var body map[string]interface{}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"detail": "invalid JSON"})
		}
		cid, _ := body["cluster_id"].(string)
		if cid == "" {
			return c.Status(400).JSON(fiber.Map{"detail": "cluster_id required"})
		}
		body["_received_at"] = time.Now().UTC().Format(time.RFC3339)

		clustersMu.Lock()
		clusters[cid] = body
		clustersMu.Unlock()

		total, _ := body["total_gpus"].(float64)
		avail, _ := body["available_gpus"].(float64)
		nodes, _ := body["nodes"].([]interface{})
		log.Printf("Heartbeat from %s: %.0f GPUs, %.0f available, %d nodes",
			cid, total, avail, len(nodes))

		return c.JSON(fiber.Map{"status": "ok", "cluster_id": cid})
	})

	app.Post("/api/clusters/provision", func(c *fiber.Ctx) error {
		var req LaunchClusterRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"detail": "invalid request"})
		}
		if req.Name == "" {
			return c.Status(400).JSON(fiber.Map{"detail": "name required"})
		}
		if req.GPUType == "" {
			req.GPUType = "A100"
		}
		if req.GPUCount == 0 {
			req.GPUCount = 8
		}
		if req.IdleMinutesToAutostop == 0 {
			req.IdleMinutesToAutostop = 60
		}

		args := []string{
			"launch", "-c", req.Name,
			"--gpus", req.GPUType + ":" + strconv.Itoa(req.GPUCount),
			"-y",
			"--idle-minutes-to-autostop", strconv.Itoa(req.IdleMinutesToAutostop),
		}
		if req.Cloud != nil && *req.Cloud != "" {
			args = append(args, "--cloud", *req.Cloud)
		}
		if req.Region != nil && *req.Region != "" {
			args = append(args, "--region", *req.Region)
		}
		if req.UseSpot {
			args = append(args, "--use-spot")
		}

		result, err := runSkyCmd(args, 600)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return c.Status(504).JSON(fiber.Map{"detail": "SkyPilot command timed out"})
			}
			return c.Status(500).JSON(fiber.Map{"detail": "command failed: " + err.Error()})
		}
		status := "launching"
		if result.Returncode != 0 {
			status = "error"
		}
		resp := fiber.Map{
			"cluster_name": req.Name,
			"status":       status,
			"output":       result.Stdout,
		}
		if result.Returncode != 0 {
			resp["error"] = result.Stderr
		}
		return c.JSON(resp)
	})

	// Provision teardown must be before :cluster_id (more specific path)
	app.Delete("/api/clusters/provision/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		result, _ := runSkyCmd([]string{"down", name, "-y"}, 300)
		status := "terminating"
		if result.Returncode != 0 {
			status = "error"
		}
		return c.JSON(fiber.Map{
			"cluster_name": name,
			"status":       status,
			"output":       result.Stdout,
		})
	})

	// DELETE provision/:name must be before :cluster_id (more specific path)
	app.Delete("/api/clusters/provision/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		result, _ := runSkyCmd([]string{"down", name, "-y"}, 300)
		status := "terminating"
		if result.Returncode != 0 {
			status = "error"
		}
		return c.JSON(fiber.Map{
			"cluster_name": name,
			"status":       status,
			"output":       result.Stdout,
		})
	})

	// Must register /provision/:name before :cluster_id so DELETE /api/clusters/provision/xyz matches correctly
	app.Delete("/api/clusters/provision/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		result, _ := runSkyCmd([]string{"down", name, "-y"}, 300)
		status := "terminating"
		if result.Returncode != 0 {
			status = "error"
		}
		return c.JSON(fiber.Map{
			"cluster_name": name,
			"status":       status,
			"output":       result.Stdout,
		})
	})

	app.Get("/api/clusters", func(c *fiber.Ctx) error {
		clustersMu.RLock()
		defer clustersMu.RUnlock()

		now := time.Now().UTC()
		var list []map[string]interface{}
		for _, state := range clusters {
			cluster := make(map[string]interface{})
			for k, v := range state {
				if k == "_received_at" {
					continue
				}
				cluster[k] = v
			}
			if rec, ok := state["_received_at"].(string); ok && rec != "" {
				if t, err := time.Parse(time.RFC3339, rec); err == nil {
					if now.Sub(t).Seconds() > staleThresholdSec {
						cluster["status"] = "stale"
					}
				}
			}
			list = append(list, cluster)
		}

		var totalGPUs, availableGPUs, totalNodes int
		for _, cl := range list {
			if v, ok := cl["total_gpus"].(float64); ok {
				totalGPUs += int(v)
			}
			if v, ok := cl["available_gpus"].(float64); ok {
				availableGPUs += int(v)
			}
			if nodes, ok := cl["nodes"].([]interface{}); ok {
				totalNodes += len(nodes)
			}
		}

		return c.JSON(fiber.Map{
			"summary": fiber.Map{
				"cluster_count":  len(list),
				"total_gpus":     totalGPUs,
				"available_gpus": availableGPUs,
				"total_nodes":    totalNodes,
			},
			"clusters": list,
		})
	})

	app.Get("/api/clusters/:cluster_id", func(c *fiber.Ctx) error {
		clusterID := c.Params("cluster_id")
		clustersMu.RLock()
		state, ok := clusters[clusterID]
		clustersMu.RUnlock()
		if !ok {
			return c.Status(404).JSON(fiber.Map{"detail": "Cluster not found"})
		}
		result := make(map[string]interface{})
		for k, v := range state {
			if k != "_received_at" {
				result[k] = v
			}
		}
		return c.JSON(result)
	})

	app.Delete("/api/clusters/:cluster_id", func(c *fiber.Ctx) error {
		clusterID := c.Params("cluster_id")
		clustersMu.Lock()
		_, ok := clusters[clusterID]
		if ok {
			delete(clusters, clusterID)
		}
		clustersMu.Unlock()
		if !ok {
			return c.Status(404).JSON(fiber.Map{"detail": "Cluster not found"})
		}
		return c.JSON(fiber.Map{"status": "removed", "cluster_id": clusterID})
	})

	// --- Service management (sky serve) ---
	app.Post("/api/services", func(c *fiber.Ctx) error {
		var req DeployServiceRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"detail": "invalid request"})
		}
		if req.Name == "" || req.ModelURI == "" {
			return c.Status(400).JSON(fiber.Map{"detail": "name and model_uri required"})
		}
		if req.Runtime == "" {
			req.Runtime = "vllm"
		}

		yamlContent, err := generateServiceYAML(&req)
		if err != nil || yamlContent == "" {
			return c.Status(400).JSON(fiber.Map{"detail": "Unsupported runtime: " + req.Runtime})
		}

		yamlPath := filepath.Join(servicesDir, req.Name+".yaml")
		if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
			return c.Status(500).JSON(fiber.Map{"detail": "failed to write YAML"})
		}
		log.Printf("Generated service YAML for %s:\n%s", req.Name, yamlContent)

		result, runErr := runSkyCmd([]string{"serve", "up", yamlPath, "-n", req.Name, "-y"}, 600)
		if errors.Is(runErr, context.DeadlineExceeded) {
			return c.Status(504).JSON(fiber.Map{"detail": "SkyPilot command timed out"})
		}
		if result.Returncode != 0 {
			return c.JSON(fiber.Map{
				"status":       "error",
				"service_name": req.Name,
				"error":        result.Stderr,
				"yaml":         yamlContent,
			})
		}
		return c.JSON(fiber.Map{
			"status":       "deploying",
			"service_name": req.Name,
			"yaml":         yamlContent,
			"output":       result.Stdout,
		})
	})

	app.Get("/api/services", func(c *fiber.Ctx) error {
		result, _ := runSkyCmd([]string{"serve", "status"}, 300)
		resp := fiber.Map{"output": result.Stdout}
		if result.Returncode != 0 {
			resp["error"] = result.Stderr
		}
		return c.JSON(resp)
	})

	app.Get("/api/services/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		result, _ := runSkyCmd([]string{"serve", "status", name}, 300)
		resp := fiber.Map{"service_name": name, "output": result.Stdout}
		if result.Returncode != 0 {
			resp["error"] = result.Stderr
		}
		return c.JSON(resp)
	})

	app.Patch("/api/services/:name/scale", func(c *fiber.Ctx) error {
		name := c.Params("name")
		var req ScaleServiceRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"detail": "invalid request"})
		}
		args := []string{"serve", "update", name}
		if req.MinReplicas != nil {
			args = append(args, "--min-replicas", strconv.Itoa(*req.MinReplicas))
		}
		if req.MaxReplicas != nil {
			args = append(args, "--max-replicas", strconv.Itoa(*req.MaxReplicas))
		}
		result, _ := runSkyCmd(args, 300)
		status := "scaling"
		if result.Returncode != 0 {
			status = "error"
		}
		return c.JSON(fiber.Map{
			"service_name": name,
			"status":       status,
			"output":       result.Stdout,
		})
	})

	app.Delete("/api/services/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		result, _ := runSkyCmd([]string{"serve", "down", name, "-y"}, 300)
		status := "deleting"
		if result.Returncode != 0 {
			status = "error"
		}
		return c.JSON(fiber.Map{
			"service_name": name,
			"status":       status,
			"output":       result.Stdout,
		})
	})

	// --- Resources and GPUs ---
	app.Get("/api/resources", func(c *fiber.Ctx) error {
		result, _ := runSkyCmd([]string{"check"}, 300)
		return c.JSON(fiber.Map{"output": result.Stdout})
	})

	app.Get("/api/gpus", func(c *fiber.Ctx) error {
		cloud := c.Query("cloud")
		args := []string{"show-gpus"}
		if cloud != "" {
			args = append(args, "--cloud", cloud)
		}
		result, _ := runSkyCmd(args, 300)
		return c.JSON(fiber.Map{"output": result.Stdout})
	})

	log.Printf("SkyPilot Bridge starting on :%s (services_dir=%s)", port, servicesDir)
	log.Fatal(app.Listen(":" + port))
}
