package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DetoServe Cluster Agent (SkyPilot-powered)
//
// Deployed via Helm chart on each onboarded cluster.
// Uses SkyPilot's Kubernetes API for GPU/node discovery
// and sends heartbeats to the DetoServe control plane.

type Config struct {
	ControlPlaneURL string
	ClusterID       string
	ClusterName     string
	ReportInterval  time.Duration
	Port            string
	APIToken        string
}

func loadConfig() Config {
	interval := 10
	fmt.Sscanf(envOr("REPORT_INTERVAL_SEC", "10"), "%d", &interval)
	return Config{
		ControlPlaneURL: envOr("CONTROL_PLANE_URL", "http://localhost:8085"),
		ClusterID:       envOr("CLUSTER_ID", ""),
		ClusterName:     envOr("CLUSTER_NAME", ""),
		ReportInterval:  time.Duration(interval) * time.Second,
		Port:            envOr("PORT", "9090"),
		APIToken:        envOr("API_TOKEN", ""),
	}
}

type Agent struct {
	cfg    Config
	client *http.Client
	state  *ClusterState
}

type ClusterState struct {
	ClusterID     string     `json:"cluster_id"`
	ClusterName   string     `json:"cluster_name"`
	Status        string     `json:"status"`
	Provider      string     `json:"provider"`
	TotalGPUs     int        `json:"total_gpus"`
	AvailableGPUs int        `json:"available_gpus"`
	GPUTypes      []GPUType  `json:"gpu_types"`
	Nodes         []NodeInfo `json:"nodes"`
	LastHeartbeat time.Time  `json:"last_heartbeat"`
}

type GPUType struct {
	Name      string `json:"name"`
	Family    string `json:"family"`
	Count     int    `json:"count"`
	Available int    `json:"available"`
}

type NodeInfo struct {
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	Role            string  `json:"role"`
	CPU             string  `json:"cpu"`
	CPUFree         float64 `json:"cpu_free"`
	MemoryGB        string  `json:"memory_gb"`
	MemoryFreeGB    string  `json:"memory_free_gb"`
	GPUType         string  `json:"gpu_type"`
	AcceleratorType string  `json:"accelerator_type"`
	GPUCount        int     `json:"gpu_count"`
	GPUAvailable    int     `json:"gpu_available"`
	GPUUsed         int     `json:"gpu_used"`
	GPUFamily       string  `json:"gpu_family"`
}

func NewAgent(cfg Config) *Agent {
	if cfg.ClusterID == "" {
		cfg.ClusterID = discoverClusterID()
	}
	if cfg.ClusterName == "" {
		cfg.ClusterName = cfg.ClusterID
	}
	return &Agent{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		state:  &ClusterState{ClusterID: cfg.ClusterID, ClusterName: cfg.ClusterName},
	}
}

func discoverClusterID() string {
	out, err := exec.Command("kubectl", "config", "current-context").Output()
	if err != nil {
		return "unknown-" + fmt.Sprintf("%d", time.Now().Unix())
	}
	return strings.TrimSpace(string(out))
}

// HeartbeatLoop discovers GPUs via SkyPilot and sends state to control plane.
func (a *Agent) HeartbeatLoop() {
	a.discover()
	a.sendHeartbeat()

	ticker := time.NewTicker(a.cfg.ReportInterval)
	defer ticker.Stop()

	for range ticker.C {
		a.discover()
		a.sendHeartbeat()
	}
}

// skyDiscoveryResult matches the JSON output of skypilot_discover.py
type skyDiscoveryResult struct {
	Nodes []struct {
		Name            string  `json:"name"`
		AcceleratorType string  `json:"accelerator_type"`
		GPUCount        int     `json:"gpu_count"`
		GPUAvailable    int     `json:"gpu_available"`
		CPU             float64 `json:"cpu"`
		CPUFree         float64 `json:"cpu_free"`
		MemoryGB        float64 `json:"memory_gb"`
		MemoryFreeGB    float64 `json:"memory_free_gb"`
		IPAddress       string  `json:"ip_address"`
		IsReady         bool    `json:"is_ready"`
	} `json:"nodes"`
	Error string `json:"error"`
}

// discoverViaSkyPilot calls the Python helper that uses SkyPilot's Kubernetes API.
func (a *Agent) discoverViaSkyPilot() ([]NodeInfo, error) {
	scriptPath := filepath.Join(exeDir(), "skypilot_discover.py")
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		scriptPath = "skypilot_discover.py"
	}

	cmd := exec.Command("python3", "-W", "ignore", scriptPath)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("skypilot_discover.py failed: %v — %s", err, string(out))
	}

	var result skyDiscoveryResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse skypilot output: %v", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("skypilot error: %s", result.Error)
	}

	var nodes []NodeInfo
	for _, n := range result.Nodes {
		status := "NotReady"
		if n.IsReady {
			status = "Ready"
		}
		role := "worker"
		if strings.Contains(n.Name, "server") || strings.Contains(n.Name, "master") || strings.Contains(n.Name, "control") {
			role = "control-plane"
		}

		gpuUsed := n.GPUCount - n.GPUAvailable
		if gpuUsed < 0 {
			gpuUsed = 0
		}

		nodes = append(nodes, NodeInfo{
			Name:            n.Name,
			Status:          status,
			Role:            role,
			CPU:             fmt.Sprintf("%.0f", n.CPU),
			CPUFree:         n.CPUFree,
			MemoryGB:        fmt.Sprintf("%.1f", n.MemoryGB),
			MemoryFreeGB:    fmt.Sprintf("%.1f", n.MemoryFreeGB),
			GPUType:         n.AcceleratorType,
			AcceleratorType: n.AcceleratorType,
			GPUCount:        n.GPUCount,
			GPUAvailable:    n.GPUAvailable,
			GPUUsed:         gpuUsed,
			GPUFamily:       gpuFamily(n.AcceleratorType),
		})
	}
	return nodes, nil
}

func gpuFamily(accType string) string {
	t := strings.ToUpper(accType)
	switch {
	case strings.Contains(t, "H100") || strings.Contains(t, "H200"):
		return "hopper"
	case strings.Contains(t, "A100") || strings.Contains(t, "A10") || strings.Contains(t, "A30") || strings.Contains(t, "A40"):
		return "ampere"
	case strings.Contains(t, "L40") || strings.Contains(t, "L4"):
		return "ada-lovelace"
	case strings.Contains(t, "V100"):
		return "volta"
	case strings.Contains(t, "T4"):
		return "turing"
	default:
		return ""
	}
}

// discover uses SkyPilot to build cluster state.
func (a *Agent) discover() {
	nodes, err := a.discoverViaSkyPilot()
	if err != nil {
		log.Printf("[discover] SkyPilot discovery failed: %v", err)
		return
	}

	a.state.Nodes = nodes
	a.state.Status = "healthy"
	a.state.Provider = detectProvider()
	a.state.LastHeartbeat = time.Now()

	totalGPUs := 0
	availGPUs := 0
	gpuMap := make(map[string]*GPUType)

	for _, n := range nodes {
		totalGPUs += n.GPUCount
		availGPUs += n.GPUAvailable
		if n.GPUType != "" {
			key := n.GPUType
			if _, ok := gpuMap[key]; !ok {
				gpuMap[key] = &GPUType{Name: key, Family: n.GPUFamily}
			}
			gpuMap[key].Count += n.GPUCount
			gpuMap[key].Available += n.GPUAvailable
		}
	}

	a.state.TotalGPUs = totalGPUs
	a.state.AvailableGPUs = availGPUs

	types := make([]GPUType, 0, len(gpuMap))
	for _, v := range gpuMap {
		types = append(types, *v)
	}
	a.state.GPUTypes = types

	log.Printf("[discover/skypilot] cluster=%s gpus=%d/%d available, nodes=%d",
		a.state.ClusterID, availGPUs, totalGPUs, len(nodes))
}

func detectProvider() string {
	ctx, _ := exec.Command("kubectl", "config", "current-context").Output()
	context := strings.TrimSpace(string(ctx))
	switch {
	case strings.HasPrefix(context, "k3d-"):
		return "k3d (local)"
	case strings.Contains(context, "eks"):
		return "AWS EKS"
	case strings.Contains(context, "gke"):
		return "GCP GKE"
	case strings.Contains(context, "aks"):
		return "Azure AKS"
	default:
		return "Kubernetes"
	}
}

// sendHeartbeat POSTs cluster state to the control plane.
func (a *Agent) sendHeartbeat() {
	if a.state.Nodes == nil {
		return
	}
	data, _ := json.Marshal(a.state)
	url := a.cfg.ControlPlaneURL + "/api/clusters/heartbeat"

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		log.Printf("[heartbeat] create request failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.APIToken)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		log.Printf("[heartbeat] send to %s failed: %v", url, err)
		return
	}
	resp.Body.Close()
	log.Printf("[heartbeat] sent -> %d", resp.StatusCode)
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func main() {
	cfg := loadConfig()
	agent := NewAgent(cfg)

	go agent.HeartbeatLoop()

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agent.state)
	})

	addr := ":" + cfg.Port
	log.Printf("DetoServe Agent starting on %s (cluster=%s, control-plane=%s, discovery=skypilot)",
		addr, cfg.ClusterID, cfg.ControlPlaneURL)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("HTTP server failed: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
