package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

// Config Store — GitOps-compatible persistent storage for function definitions.
//
// Every function and deployment config is saved to disk as JSON files.
// The storage directory can be a Git repo, so:
//   - ArgoCD or Flux watches the repo
//   - Any manual edit in Git triggers re-sync
//   - Full audit trail via git log
//   - Easy rollback via git revert
//
// Directory layout:
//   configs/
//     functions/
//       fn-123456.json
//       fn-789012.json
//     instances/
//       inst-123456.json

type Config struct {
	Port       string
	StorageDir string
}

func loadConfig() Config {
	return Config{
		Port:       envOr("PORT", "8087"),
		StorageDir: envOr("STORAGE_DIR", "/data/configs"),
	}
}

func main() {
	cfg := loadConfig()

	os.MkdirAll(filepath.Join(cfg.StorageDir, "functions"), 0755)
	os.MkdirAll(filepath.Join(cfg.StorageDir, "instances"), 0755)

	app := fiber.New()
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Content-Type, Authorization",
	}))

	// Save function config
	app.Post("/api/configs/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		data := c.Body()

		// Pretty-print JSON for readability in Git
		var raw interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid JSON"})
		}
		pretty, _ := json.MarshalIndent(raw, "", "  ")

		path := filepath.Join(cfg.StorageDir, "functions", id+".json")
		if err := os.WriteFile(path, pretty, 0644); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "write failed"})
		}

		log.Printf("Config saved: %s", path)
		return c.JSON(fiber.Map{"status": "saved", "path": path})
	})

	// Save instance config
	app.Post("/api/configs/instances/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		data := c.Body()

		var raw interface{}
		json.Unmarshal(data, &raw)
		pretty, _ := json.MarshalIndent(raw, "", "  ")

		path := filepath.Join(cfg.StorageDir, "instances", id+".json")
		if err := os.WriteFile(path, pretty, 0644); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "write failed"})
		}

		return c.JSON(fiber.Map{"status": "saved", "path": path})
	})

	// List all saved function configs
	app.Get("/api/configs", func(c *fiber.Ctx) error {
		funcDir := filepath.Join(cfg.StorageDir, "functions")
		entries, err := os.ReadDir(funcDir)
		if err != nil {
			return c.JSON([]interface{}{})
		}

		var configs []interface{}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(funcDir, e.Name()))
			if err != nil {
				continue
			}
			var obj interface{}
			json.Unmarshal(data, &obj)
			configs = append(configs, obj)
		}
		return c.JSON(configs)
	})

	// Get specific config
	app.Get("/api/configs/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		path := filepath.Join(cfg.StorageDir, "functions", id+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		var obj interface{}
		json.Unmarshal(data, &obj)
		return c.JSON(obj)
	})

	// Delete config
	app.Delete("/api/configs/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		path := filepath.Join(cfg.StorageDir, "functions", id+".json")
		os.Remove(path)
		return c.JSON(fiber.Map{"status": "deleted"})
	})

	// List all instance configs
	app.Get("/api/configs/instances", func(c *fiber.Ctx) error {
		instDir := filepath.Join(cfg.StorageDir, "instances")
		entries, err := os.ReadDir(instDir)
		if err != nil {
			return c.JSON([]interface{}{})
		}

		var configs []interface{}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") { continue }
			data, _ := os.ReadFile(filepath.Join(instDir, e.Name()))
			var obj interface{}
			json.Unmarshal(data, &obj)
			configs = append(configs, obj)
		}
		return c.JSON(configs)
	})

	// Git commit endpoint — triggers a git commit of all configs
	app.Post("/api/configs/commit", func(c *fiber.Ctx) error {
		var body struct {
			Message string `json:"message"`
		}
		c.BodyParser(&body)
		if body.Message == "" {
			body.Message = fmt.Sprintf("Auto-save configs at %s", time.Now().Format(time.RFC3339))
		}

		// In production: exec `git add . && git commit -m "..." && git push`
		// inside the StorageDir. ArgoCD watches the repo and syncs.
		log.Printf("Git commit requested: %s", body.Message)

		return c.JSON(fiber.Map{
			"status":  "committed",
			"message": body.Message,
			"note":    "In production, this triggers git add + commit + push",
		})
	})

	// Health
	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	log.Printf("Config Store starting on :%s (storage=%s)", cfg.Port, cfg.StorageDir)
	log.Fatal(app.Listen(":" + cfg.Port))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}
