package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"time"
)

// Autoscaler Controller monitors cross-cluster inference metrics
// and generates scaling decisions. It works alongside KEDA (per-pod HPA)
// by handling cluster-level and cross-cluster scaling.

type ScalingPolicy struct {
	DeploymentName  string  `json:"deployment_name"`
	TenantID        string  `json:"tenant_id"`
	MinReplicas     int     `json:"min_replicas"`
	MaxReplicas     int     `json:"max_replicas"`
	Metric          string  `json:"metric"`          // queue_depth, latency, gpu_util
	TargetValue     float64 `json:"target_value"`
	ScaleUpCooldown time.Duration
	ScaleDownCooldown time.Duration
	LastScaleUp     time.Time
	LastScaleDown   time.Time
}

type MetricSnapshot struct {
	ClusterID       string  `json:"cluster_id"`
	DeploymentName  string  `json:"deployment_name"`
	QueueDepth      float64 `json:"queue_depth"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	GPUUtilization  float64 `json:"gpu_utilization"`
	CurrentReplicas int     `json:"current_replicas"`
	ReadyReplicas   int     `json:"ready_replicas"`
}

type ScalingDecision struct {
	ClusterID      string `json:"cluster_id"`
	DeploymentName string `json:"deployment_name"`
	Action         string `json:"action"` // scale_up, scale_down, none
	CurrentReplicas int   `json:"current_replicas"`
	DesiredReplicas int   `json:"desired_replicas"`
	Reason         string `json:"reason"`
}

type AutoscalerController struct {
	prometheusURL string
	policies      map[string]*ScalingPolicy
}

func NewAutoscalerController() *AutoscalerController {
	return &AutoscalerController{
		prometheusURL: envOr("PROMETHEUS_URL", "http://prometheus.monitoring:9090"),
		policies:      make(map[string]*ScalingPolicy),
	}
}

// Evaluate checks all registered policies against current metrics.
func (ac *AutoscalerController) Evaluate(snapshots []MetricSnapshot) []ScalingDecision {
	var decisions []ScalingDecision

	for _, snap := range snapshots {
		key := snap.ClusterID + "/" + snap.DeploymentName
		policy, ok := ac.policies[key]
		if !ok {
			continue
		}

		decision := ac.evaluateOne(policy, snap)
		if decision.Action != "none" {
			decisions = append(decisions, decision)
		}
	}

	return decisions
}

func (ac *AutoscalerController) evaluateOne(policy *ScalingPolicy, snap MetricSnapshot) ScalingDecision {
	decision := ScalingDecision{
		ClusterID:       snap.ClusterID,
		DeploymentName:  snap.DeploymentName,
		CurrentReplicas: snap.CurrentReplicas,
		DesiredReplicas: snap.CurrentReplicas,
		Action:          "none",
	}

	var currentValue float64
	switch policy.Metric {
	case "queue_depth":
		currentValue = snap.QueueDepth
	case "latency":
		currentValue = snap.AvgLatencyMs
	case "gpu_utilization":
		currentValue = snap.GPUUtilization
	default:
		currentValue = snap.QueueDepth
	}

	ratio := currentValue / policy.TargetValue
	desired := int(math.Ceil(float64(snap.CurrentReplicas) * ratio))

	// Clamp
	if desired < policy.MinReplicas {
		desired = policy.MinReplicas
	}
	if desired > policy.MaxReplicas {
		desired = policy.MaxReplicas
	}

	now := time.Now()

	if desired > snap.CurrentReplicas {
		if now.Sub(policy.LastScaleUp) < policy.ScaleUpCooldown {
			return decision
		}
		decision.Action = "scale_up"
		decision.DesiredReplicas = desired
		decision.Reason = fmt.Sprintf("%s=%.1f > target=%.1f, ratio=%.2f",
			policy.Metric, currentValue, policy.TargetValue, ratio)
		policy.LastScaleUp = now
	} else if desired < snap.CurrentReplicas {
		if now.Sub(policy.LastScaleDown) < policy.ScaleDownCooldown {
			return decision
		}
		decision.Action = "scale_down"
		decision.DesiredReplicas = desired
		decision.Reason = fmt.Sprintf("%s=%.1f < target=%.1f, ratio=%.2f",
			policy.Metric, currentValue, policy.TargetValue, ratio)
		policy.LastScaleDown = now
	}

	return decision
}

func (ac *AutoscalerController) RegisterPolicy(clusterID string, policy *ScalingPolicy) {
	key := clusterID + "/" + policy.DeploymentName
	if policy.ScaleUpCooldown == 0 {
		policy.ScaleUpCooldown = 30 * time.Second
	}
	if policy.ScaleDownCooldown == 0 {
		policy.ScaleDownCooldown = 5 * time.Minute
	}
	ac.policies[key] = policy
}

// --- Server ---

func main() {
	port := envOr("PORT", "8084")
	controller := NewAutoscalerController()

	mux := http.NewServeMux()

	// Register a scaling policy
	mux.HandleFunc("POST /api/policies", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClusterID string        `json:"cluster_id"`
			Policy    ScalingPolicy `json:"policy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		controller.RegisterPolicy(body.ClusterID, &body.Policy)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
	})

	// Evaluate metrics and return scaling decisions
	mux.HandleFunc("POST /api/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var snapshots []MetricSnapshot
		if err := json.NewDecoder(r.Body).Decode(&snapshots); err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		decisions := controller.Evaluate(snapshots)
		json.NewEncoder(w).Encode(decisions)
	})

	// Health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("Autoscaler Controller starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
