package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	portCounter   int = 9100
	portCounterMu sync.Mutex
)

func devMode() bool {
	return os.Getenv("DEV_MODE") == "true"
}

// resolveGPUName maps user-facing GPU names to SkyPilot accelerator names.
func resolveGPUName(gpuType string) string {
	m := map[string]string{
		"A100":       "A100-80GB",
		"H100":       "H100-80GB",
		"A100-80GB":  "A100-80GB",
		"H100-80GB":  "H100-80GB",
		"L40S":       "L40S",
		"V100":       "V100",
	}
	if v, ok := m[gpuType]; ok {
		return v
	}
	return gpuType
}

// buildRunScript returns the Python inference server script for SkyPilot.
func buildRunScript(modelURI, runtime string, gpuCount int) string {
	return fmt.Sprintf(`python3 -c "
import http.server, json, time
class H(http.server.BaseHTTPRequestHandler):
    def _respond(self, data, code=200):
        self.send_response(code)
        self.send_header('Content-Type','application/json')
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())
    def do_GET(self):
        if self.path == '/health':
            self._respond({'status':'healthy','model':'%s','runtime':'%s','gpu_count':%d})
        elif self.path == '/v1/models':
            self._respond({'object':'list','data':[{'id':'%s','object':'model','owned_by':'detoserve'}]})
        else:
            self._respond({'message':'DetoServe inference','model':'%s'})
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = json.loads(self.rfile.read(length)) if length else {}
        if '/v1/chat/completions' in self.path:
            msgs = body.get('messages', [])
            last = msgs[-1]['content'] if msgs else 'hello'
            self._respond({
                'id': 'chatcmpl-detoserve',
                'object': 'chat.completion',
                'created': int(time.time()),
                'model': '%s',
                'choices': [{
                    'index': 0,
                    'message': {'role': 'assistant', 'content': '[DetoServe %s on %dx GPU] Echo: ' + last},
                    'finish_reason': 'stop'
                }],
                'usage': {'prompt_tokens': len(last.split()), 'completion_tokens': 10, 'total_tokens': len(last.split()) + 10}
            })
        elif '/v1/completions' in self.path:
            prompt = body.get('prompt', '')
            self._respond({
                'id': 'cmpl-detoserve',
                'object': 'text_completion',
                'created': int(time.time()),
                'model': '%s',
                'choices': [{'text': '[DetoServe] ' + prompt + '...continued', 'index': 0, 'finish_reason': 'stop'}]
            })
        else:
            self._respond({'message': 'use /v1/chat/completions or /v1/completions'})
    def log_message(self, *a): pass
print('Inference server for %s on :8080')
http.server.HTTPServer(('',8080),H).serve_forever()
"`, modelURI, runtime, gpuCount, modelURI, modelURI, modelURI, runtime, gpuCount, modelURI, modelURI)
}

// buildSkyYAML generates the SkyPilot task YAML.
func buildSkyYAML(modelURI, runtime string, gpuCount int, gpuType, cloud string) string {
	runScript := buildRunScript(modelURI, runtime, gpuCount)
	// Escape for YAML: pipe literal - preserve newlines, avoid trailing newline issues
	runLines := strings.Split(strings.TrimSpace(runScript), "\n")
	var sb strings.Builder
	for _, line := range runLines {
		sb.WriteString("  ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	runYAML := sb.String()

	resources := fmt.Sprintf("  accelerators: %s:%d\n  ports: 8080\n", gpuType, gpuCount)
	if cloud == "kubernetes" {
		resources += "  cloud: kubernetes\n"
	}
	if devMode() {
		resources += "  cpus: 1\n  memory: 2\n"
	}

	return fmt.Sprintf(`name: detoserve-task
resources:
%s
run: |
%s`, resources, runYAML)
}

// getSkyPilotEndpoint discovers the LB service and starts port-forward.
func getSkyPilotEndpoint(skyCluster string, maxRetries int) string {
	clusterSlug := strings.ReplaceAll(skyCluster, "-", "")
	for attempt := 0; attempt < maxRetries; attempt++ {
		cmd := exec.Command("kubectl", "get", "svc", "-n", "default", "-o", "json")
		cmd.Stderr = nil
		out, err := cmd.Output()
		if err != nil {
			log.Printf("[skypilot] kubectl get svc failed (attempt %d/%d): %v", attempt+1, maxRetries, err)
			time.Sleep(10 * time.Second)
			continue
		}

		var svcList struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Ports []struct {
						Port int `json:"port"`
					} `json:"ports"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := json.Unmarshal(out, &svcList); err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		var lbSvcName string
		for _, svc := range svcList.Items {
			name := svc.Metadata.Name
			if !strings.Contains(name, "skypilot-lb") {
				continue
			}
			if !strings.Contains(strings.ReplaceAll(name, "-", ""), clusterSlug) {
				continue
			}
			for _, p := range svc.Spec.Ports {
				if p.Port == 8080 {
					lbSvcName = name
					break
				}
			}
			if lbSvcName != "" {
				break
			}
		}

		if lbSvcName == "" {
			log.Printf("[skypilot] %s: LB service not found (attempt %d/%d)", skyCluster, attempt+1, maxRetries)
			time.Sleep(10 * time.Second)
			continue
		}

		portCounterMu.Lock()
		localPort := portCounter
		portCounter++
		portCounterMu.Unlock()

		pf := exec.Command("kubectl", "port-forward", "svc/"+lbSvcName,
			fmt.Sprintf("%d:8080", localPort), "-n", "default")
		pf.Stdout = nil
		pf.Stderr = nil
		if err := pf.Start(); err != nil {
			log.Printf("[skypilot] port-forward start failed: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		// Don't wait - let it run in background
		go func() { _ = pf.Wait() }()
		time.Sleep(2 * time.Second)

		endpoint := fmt.Sprintf("http://localhost:%d", localPort)
		log.Printf("[skypilot] %s -> %s -> %s", skyCluster, lbSvcName, endpoint)
		return endpoint
	}
	log.Printf("[skypilot] %s: gave up after %d retries", skyCluster, maxRetries)
	return ""
}

// DeployInstanceFunc is called to update instance fields after deploy/discovery.
type DeployInstanceFunc func(endpoint, status, cluster string)

// DeploySkyPilot deploys an instance via sky launch. Runs in a goroutine.
func DeploySkyPilot(instID string, fn *Function, cloud string, onUpdate DeployInstanceFunc) {
	gpuCount := fn.Resources.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	gpuType := resolveGPUName(fn.Resources.GPUType)
	modelURI := fn.ModelURI
	if modelURI == "" {
		modelURI = "unknown"
	}
	runtime := fn.Runtime
	if runtime == "" {
		runtime = "vllm"
	}
	fnName := strings.ReplaceAll(strings.ToLower(fn.Name), " ", "-")
	if fnName == "" {
		fnName = "func"
	}
	instSuffix := instID
	if len(instSuffix) > 6 {
		instSuffix = instSuffix[len(instSuffix)-6:]
	}
	skyCluster := fmt.Sprintf("detoserve-%s-%s", fnName, instSuffix)
	onUpdate("", "deploying", skyCluster)

	yamlContent := buildSkyYAML(modelURI, runtime, gpuCount, gpuType, cloud)
	tmpFile, err := os.CreateTemp("", "skypilot-*.yaml")
	if err != nil {
		log.Printf("[skypilot] failed to create temp file: %v", err)
		onUpdate("", "error", skyCluster)
		return
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(yamlContent); err != nil {
		log.Printf("[skypilot] failed to write YAML: %v", err)
		onUpdate("", "error", skyCluster)
		return
	}
	tmpFile.Close()

	log.Printf("[skypilot] Launching %s with %dx %s", skyCluster, gpuCount, gpuType)
	cmd := exec.Command("sky", "launch", tmpFile.Name(), "--cluster", skyCluster, "-y")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		log.Printf("[skypilot] sky launch failed for %s: %v", skyCluster, err)
		onUpdate("", "error", skyCluster)
		return
	}
	log.Printf("[skypilot] %s launch succeeded", skyCluster)

	endpoint := getSkyPilotEndpoint(skyCluster, 12)
	if endpoint == "" {
		onUpdate("", "degraded", skyCluster)
		return
	}
	onUpdate(endpoint, "running", skyCluster)
	log.Printf("[skypilot] %s is running — endpoint: %s", skyCluster, endpoint)
}

// SkyDown tears down a SkyPilot cluster. Runs in a goroutine.
func SkyDown(clusterName string) {
	if clusterName == "" {
		return
	}
	cmd := exec.Command("sky", "down", clusterName, "-y")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		log.Printf("[skypilot] sky down %s error: %v", clusterName, err)
		return
	}
	log.Printf("[skypilot] %s terminated", clusterName)
}
