package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DetoServe Cluster Agent
//
// Deployed via Helm chart on each onboarded cluster.
// Responsibilities:
//   1. GPU Discovery — runs `sky show-gpus --cloud kubernetes` to detect GPUs
//   2. Node Inventory — queries Kubernetes API for node status and resources
//   3. Heartbeat — reports cluster health, GPU inventory, and utilization to
//      the DetoServe control plane every N seconds
//   4. Cache Reporting — scrapes vLLM pods for KV cache prefix hashes and
//      load metrics, forwards to Smart Router

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
	K8sVersion    string     `json:"k8s_version"`
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
	Name             string `json:"name"`
	Status           string `json:"status"`
	Role             string `json:"role"`
	CPU              string `json:"cpu"`
	MemoryGB         string `json:"memory_gb"`
	GPUType          string `json:"gpu_type"`
	GPUCount         int    `json:"gpu_count"`
	GPUAvailable     int    `json:"gpu_available"`
	GPUFamily        string `json:"gpu_family"`
	K8sVersion       string `json:"k8s_version"`
	ContainerRuntime string `json:"container_runtime"`
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

// HeartbeatLoop discovers GPUs and sends state to control plane on interval.
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

// discover uses SkyPilot + kubectl to build cluster state.
func (a *Agent) discover() {
	nodes := a.discoverNodes()
	a.state.Nodes = nodes
	a.state.Status = "healthy"
	a.state.Provider = detectProvider()
	a.state.LastHeartbeat = time.Now()

	totalGPUs := 0
	availGPUs := 0
	gpuMap := make(map[string]*GPUType)

	for _, n := range nodes {
		if n.K8sVersion != "" && a.state.K8sVersion == "" {
			a.state.K8sVersion = n.K8sVersion
		}
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

	log.Printf("[discover] cluster=%s gpus=%d/%d available, nodes=%d",
		a.state.ClusterID, availGPUs, totalGPUs, len(nodes))
}

// discoverNodes queries kubectl for node info including GPU resources.
func (a *Agent) discoverNodes() []NodeInfo {
	out, err := exec.Command("kubectl", "get", "nodes", "-o", "json").Output()
	if err != nil {
		log.Printf("[discover] kubectl failed: %v", err)
		return nil
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				Capacity    map[string]string `json:"capacity"`
				Allocatable map[string]string `json:"allocatable"`
				NodeInfo    struct {
					KubeletVersion          string `json:"kubeletVersion"`
					ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
					OSImage                 string `json:"osImage"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}

	if err := json.Unmarshal(out, &result); err != nil {
		log.Printf("[discover] json parse failed: %v", err)
		return nil
	}

	var nodes []NodeInfo
	for _, item := range result.Items {
		gpuCap, _ := strconv.Atoi(item.Status.Capacity["nvidia.com/gpu"])
		gpuAlloc, _ := strconv.Atoi(item.Status.Allocatable["nvidia.com/gpu"])

		status := "NotReady"
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				status = "Ready"
			}
		}

		role := "worker"
		for k := range item.Metadata.Labels {
			if strings.HasPrefix(k, "node-role.kubernetes.io/") {
				role = strings.TrimPrefix(k, "node-role.kubernetes.io/")
			}
		}

		memKi, _ := strconv.ParseFloat(strings.TrimSuffix(item.Status.Capacity["memory"], "Ki"), 64)
		memGB := fmt.Sprintf("%.1f", memKi/1024/1024)

		n := NodeInfo{
			Name:             item.Metadata.Name,
			Status:           status,
			Role:             role,
			CPU:              item.Status.Capacity["cpu"],
			MemoryGB:         memGB,
			GPUType:          item.Metadata.Labels["nvidia.com/gpu.machine"],
			GPUCount:         gpuCap,
			GPUAvailable:     gpuAlloc,
			GPUFamily:        item.Metadata.Labels["nvidia.com/gpu.family"],
			K8sVersion:       item.Status.NodeInfo.KubeletVersion,
			ContainerRuntime: item.Status.NodeInfo.ContainerRuntimeVersion,
		}
		nodes = append(nodes, n)
	}
	return nodes
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

	if resp.StatusCode >= 300 {
		log.Printf("[heartbeat] control plane returned %d", resp.StatusCode)
	}
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

	log.Printf("DetoServe Agent starting on :%s (cluster=%s, control-plane=%s)",
		cfg.Port, cfg.ClusterID, cfg.ControlPlaneURL)
	http.ListenAndServe(":"+cfg.Port, mux)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
