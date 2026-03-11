import React, { useState, useEffect, useCallback } from 'react'

const API = '/api'

// In-memory store for demo mode (when backend is not running)
let _localFunctions = []
let _localInstances = []
let _idCounter = 1

function App() {
  const [page, setPage] = useState('functions')
  const [functions, setFunctions] = useState([])
  const [instances, setInstances] = useState([])
  const [selectedFn, setSelectedFn] = useState(null)
  const [showCreate, setShowCreate] = useState(false)
  const [showDeploy, setShowDeploy] = useState(false)

  const fetchFunctions = useCallback(async () => {
    try {
      const res = await fetch(`${API}/functions`)
      if (!res.ok) throw new Error()
      const data = await res.json()
      setFunctions(data || [])
    } catch {
      setFunctions(_localFunctions)
    }
  }, [])

  const fetchInstances = useCallback(async (fnId) => {
    try {
      const url = fnId ? `${API}/instances?function_id=${fnId}` : `${API}/instances`
      const res = await fetch(url)
      if (!res.ok) throw new Error()
      const data = await res.json()
      setInstances(data || [])
    } catch {
      const filtered = fnId
        ? _localInstances.filter(i => i.function_id === fnId)
        : _localInstances
      setInstances(filtered)
    }
  }, [])

  useEffect(() => { fetchFunctions(); fetchInstances() }, [])

  const handleCreate = async (fn) => {
    try {
      const res = await fetch(`${API}/functions`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(fn),
      })
      if (!res.ok) throw new Error()
    } catch {
      const newFn = {
        ...fn,
        id: `fn-${_idCounter++}`,
        version: 'v1',
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      }
      _localFunctions = [..._localFunctions, newFn]
    }
    setShowCreate(false)
    fetchFunctions()
  }

  const handleDeploy = async (fnId, deployConfig) => {
    const fn = _localFunctions.find(f => f.id === fnId) || functions.find(f => f.id === fnId)
    try {
      const res = await fetch(`${API}/functions/${fnId}/deploy`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(deployConfig),
      })
      if (!res.ok) throw new Error()
    } catch {
      const inst = {
        id: `inst-${_idCounter++}`,
        function_id: fnId,
        function_name: fn?.name || 'unknown',
        ...deployConfig,
        status: 'deploying',
        replicas: fn?.scaling?.min_replicas || 2,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      }
      _localInstances = [..._localInstances, inst]
      setTimeout(() => {
        const idx = _localInstances.findIndex(i => i.id === inst.id)
        if (idx >= 0) _localInstances[idx] = { ..._localInstances[idx], status: 'running' }
      }, 2000)
    }
    setShowDeploy(false)
    fetchInstances(fnId)
  }

  const handleDelete = async (fnId) => {
    try {
      const res = await fetch(`${API}/functions/${fnId}`, { method: 'DELETE' })
      if (!res.ok) throw new Error()
    } catch {
      _localFunctions = _localFunctions.filter(f => f.id !== fnId)
    }
    setSelectedFn(null)
    fetchFunctions()
  }

  const handleDeleteInstance = async (instId) => {
    try {
      const res = await fetch(`${API}/instances/${instId}`, { method: 'DELETE' })
      if (!res.ok) throw new Error()
    } catch {
      _localInstances = _localInstances.filter(i => i.id !== instId)
    }
    if (selectedFn) fetchInstances(selectedFn.id)
  }

  return (
    <div className="app">
      <nav className="topnav">
        <div className="topnav-brand">DETOSERVE</div>
        <div className="topnav-tabs">
          <button className={`topnav-tab ${page === 'functions' ? 'active' : ''}`}
            onClick={() => { setPage('functions'); setSelectedFn(null) }}>Functions</button>
          <button className={`topnav-tab ${page === 'instances' ? 'active' : ''}`}
            onClick={() => { setPage('instances'); fetchInstances() }}>Instances</button>
          <button className={`topnav-tab ${page === 'clusters' ? 'active' : ''}`}
            onClick={() => setPage('clusters')}>Clusters</button>
        </div>
      </nav>

      <main className="main">
        {page === 'functions' && !selectedFn && (
          <FunctionsPage
            functions={functions}
            onSelect={(fn) => { setSelectedFn(fn); fetchInstances(fn.id) }}
            onCreate={() => setShowCreate(true)}
          />
        )}

        {page === 'functions' && selectedFn && (
          <FunctionDetail
            fn={selectedFn}
            instances={instances}
            onBack={() => setSelectedFn(null)}
            onDeploy={() => setShowDeploy(true)}
            onDelete={() => handleDelete(selectedFn.id)}
            onDeleteInstance={handleDeleteInstance}
          />
        )}

        {page === 'instances' && (
          <InstancesPage instances={instances} />
        )}

        {page === 'clusters' && (
          <ClustersPage />
        )}
      </main>

      {showCreate && (
        <CreateFunctionModal
          onClose={() => setShowCreate(false)}
          onSubmit={handleCreate}
        />
      )}

      {showDeploy && selectedFn && (
        <DeployModal
          fn={selectedFn}
          onClose={() => setShowDeploy(false)}
          onSubmit={(cfg) => handleDeploy(selectedFn.id, cfg)}
        />
      )}
    </div>
  )
}

function FunctionsPage({ functions, onSelect, onCreate }) {
  return (
    <>
      <div className="page-header">
        <div>
          <div className="page-title">Functions</div>
          <div className="page-subtitle">Define once, deploy anywhere</div>
        </div>
        <button className="btn btn-primary" onClick={onCreate}>+ New Function</button>
      </div>

      {functions.length === 0 ? (
        <div className="empty">
          <div className="empty-icon">f(x)</div>
          <div className="empty-text">No functions yet. Create one to get started.</div>
          <button className="btn btn-primary" onClick={onCreate}>Create Function</button>
        </div>
      ) : (
        <div className="card-grid">
          {functions.map(fn => (
            <div key={fn.id} className="card" onClick={() => onSelect(fn)}>
              <div className="card-header">
                <span className="card-name">{fn.name}</span>
                <span className="tag runtime">{fn.runtime}</span>
              </div>
              <div style={{ fontSize: 13, color: 'var(--text-dim)', marginBottom: 8 }}>
                {fn.description || fn.model_uri}
              </div>
              <div className="card-meta">
                <span className="tag gpu">{fn.resources?.gpu_count || 1}x {fn.resources?.gpu_type || 'GPU'}</span>
                <span className="tag">{fn.quantization || 'fp16'}</span>
                <span className="tag">{fn.scaling?.min_replicas || 1}-{fn.scaling?.max_replicas || 10} replicas</span>
              </div>
            </div>
          ))}
        </div>
      )}
    </>
  )
}

function FunctionDetail({ fn, instances, onBack, onDeploy, onDelete, onDeleteInstance }) {
  return (
    <>
      <div className="page-header">
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <button className="btn btn-sm" onClick={onBack}>Back</button>
          <div>
            <div className="page-title">{fn.name}</div>
            <div className="page-subtitle">{fn.id} / {fn.version}</div>
          </div>
        </div>
        <div className="btn-group">
          <button className="btn btn-primary" onClick={onDeploy}>Deploy Instance</button>
          <button className="btn btn-danger" onClick={onDelete}>Delete</button>
        </div>
      </div>

      <div className="detail-section">
        <div className="detail-section-title">Configuration</div>
        <div className="detail-grid">
          <span className="detail-key">Runtime</span>
          <span className="detail-val">{fn.runtime}</span>
          <span className="detail-key">Model URI</span>
          <span className="detail-val" style={{ wordBreak: 'break-all' }}>{fn.model_uri}</span>
          <span className="detail-key">Quantization</span>
          <span className="detail-val">{fn.quantization || 'none'}</span>
          <span className="detail-key">GPU</span>
          <span className="detail-val">{fn.resources?.gpu_count}x {fn.resources?.gpu_type}</span>
          <span className="detail-key">Tensor Parallel</span>
          <span className="detail-val">{fn.resources?.tensor_parallel}</span>
          <span className="detail-key">Max Model Len</span>
          <span className="detail-val">{fn.resources?.max_model_len}</span>
          <span className="detail-key">Scaling</span>
          <span className="detail-val">{fn.scaling?.min_replicas}-{fn.scaling?.max_replicas} replicas on {fn.scaling?.metric}</span>
          <span className="detail-key">Prefix Caching</span>
          <span className="detail-val">{fn.routing?.prefix_caching ? 'Enabled' : 'Disabled'}</span>
          <span className="detail-key">Session Affinity</span>
          <span className="detail-val">{fn.routing?.session_affinity ? 'Enabled' : 'Disabled'}</span>
        </div>
      </div>

      <div className="detail-section">
        <div className="detail-section-title">Instances ({instances.length})</div>
        {instances.length === 0 ? (
          <div style={{ color: 'var(--text-dim)', fontSize: 13 }}>
            No instances deployed. Click "Deploy Instance" to create one.
          </div>
        ) : (
          <div className="instance-list">
            {instances.map(inst => (
              <div key={inst.id} className="instance-card">
                <div className="instance-info">
                  <StatusBadge status={inst.status} />
                  <div>
                    <div style={{ fontWeight: 500, fontSize: 14 }}>{inst.id}</div>
                    <div style={{ fontSize: 12, color: 'var(--text-dim)' }}>
                      {inst.cluster || inst.cloud || 'auto'} / {inst.region || 'auto'} / {inst.tenant_id}
                      {inst.use_spot && ' (spot)'}
                    </div>
                  </div>
                </div>
                <div className="btn-group">
                  <span className="tag instances">{inst.replicas} replicas</span>
                  <button className="btn btn-sm btn-danger" onClick={() => onDeleteInstance(inst.id)}>Remove</button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  )
}

function InstancesPage({ instances }) {
  return (
    <>
      <div className="page-header">
        <div>
          <div className="page-title">All Instances</div>
          <div className="page-subtitle">Running deployments across clusters</div>
        </div>
      </div>
      {instances.length === 0 ? (
        <div className="empty">
          <div className="empty-icon">-</div>
          <div className="empty-text">No instances running.</div>
        </div>
      ) : (
        <div className="instance-list">
          {instances.map(inst => (
            <div key={inst.id} className="instance-card">
              <div className="instance-info">
                <StatusBadge status={inst.status} />
                <div>
                  <div style={{ fontWeight: 500 }}>{inst.function_name}</div>
                  <div style={{ fontSize: 12, color: 'var(--text-dim)' }}>
                    {inst.id} / {inst.cluster || inst.cloud || 'auto'} / {inst.tenant_id}
                  </div>
                </div>
              </div>
              <span className="tag instances">{inst.replicas} replicas</span>
            </div>
          ))}
        </div>
      )}
    </>
  )
}

function StatusBadge({ status }) {
  return (
    <span className={`status ${status}`}>
      <span className="status-dot" />
      {status}
    </span>
  )
}

function CreateFunctionModal({ onClose, onSubmit }) {
  const [form, setForm] = useState({
    name: '', description: '', runtime: 'vllm', model_uri: '',
    quantization: 'fp16',
    resources: { gpu_type: 'A100', gpu_count: 4, tensor_parallel: 4, max_model_len: 8192 },
    scaling: { min_replicas: 2, max_replicas: 20, metric: 'queue_depth', target_value: 10 },
    routing: { prefix_caching: true, session_affinity: true },
  })

  const set = (key, val) => setForm(f => ({ ...f, [key]: val }))
  const setRes = (key, val) => setForm(f => ({ ...f, resources: { ...f.resources, [key]: val } }))
  const setScale = (key, val) => setForm(f => ({ ...f, scaling: { ...f.scaling, [key]: val } }))

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={e => e.stopPropagation()}>
        <div className="modal-title">Create Function</div>

        <div className="form-group">
          <label className="form-label">Name</label>
          <input className="form-input" placeholder="llama-70b-chat" value={form.name}
            onChange={e => set('name', e.target.value)} />
        </div>

        <div className="form-group">
          <label className="form-label">Description</label>
          <input className="form-input" placeholder="Llama 3.1 70B for chat completions"
            value={form.description} onChange={e => set('description', e.target.value)} />
        </div>

        <div className="form-group">
          <label className="form-label">Model URI</label>
          <input className="form-input" placeholder="meta-llama/Llama-3.1-70B-Instruct"
            value={form.model_uri} onChange={e => set('model_uri', e.target.value)} />
        </div>

        <div className="form-row">
          <div className="form-group">
            <label className="form-label">Runtime</label>
            <select className="form-select" value={form.runtime}
              onChange={e => set('runtime', e.target.value)}>
              <option value="vllm">vLLM</option>
              <option value="triton">Triton</option>
              <option value="dynamo">Dynamo</option>
              <option value="custom">Custom</option>
            </select>
          </div>
          <div className="form-group">
            <label className="form-label">Quantization</label>
            <select className="form-select" value={form.quantization}
              onChange={e => set('quantization', e.target.value)}>
              <option value="fp32">FP32</option>
              <option value="fp16">FP16</option>
              <option value="bf16">BF16</option>
              <option value="fp8">FP8</option>
              <option value="int8">INT8</option>
              <option value="int4">INT4</option>
            </select>
          </div>
        </div>

        <div className="form-row-3">
          <div className="form-group">
            <label className="form-label">GPU Type</label>
            <select className="form-select" value={form.resources.gpu_type}
              onChange={e => setRes('gpu_type', e.target.value)}>
              <option value="A100">A100</option>
              <option value="H100">H100</option>
              <option value="L40S">L40S</option>
              <option value="A10G">A10G</option>
            </select>
          </div>
          <div className="form-group">
            <label className="form-label">GPU Count</label>
            <input className="form-input" type="number" min="1" max="16"
              value={form.resources.gpu_count}
              onChange={e => setRes('gpu_count', parseInt(e.target.value) || 1)} />
          </div>
          <div className="form-group">
            <label className="form-label">Max Model Len</label>
            <input className="form-input" type="number"
              value={form.resources.max_model_len}
              onChange={e => setRes('max_model_len', parseInt(e.target.value) || 8192)} />
          </div>
        </div>

        <div className="form-row-3">
          <div className="form-group">
            <label className="form-label">Min Replicas</label>
            <input className="form-input" type="number" min="0"
              value={form.scaling.min_replicas}
              onChange={e => setScale('min_replicas', parseInt(e.target.value) || 1)} />
          </div>
          <div className="form-group">
            <label className="form-label">Max Replicas</label>
            <input className="form-input" type="number" min="1"
              value={form.scaling.max_replicas}
              onChange={e => setScale('max_replicas', parseInt(e.target.value) || 10)} />
          </div>
          <div className="form-group">
            <label className="form-label">Scale Metric</label>
            <select className="form-select" value={form.scaling.metric}
              onChange={e => setScale('metric', e.target.value)}>
              <option value="queue_depth">Queue Depth</option>
              <option value="latency">Latency</option>
              <option value="gpu_utilization">GPU Utilization</option>
            </select>
          </div>
        </div>

        <div className="form-actions">
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" onClick={() => onSubmit(form)}
            disabled={!form.name || !form.model_uri}>Create Function</button>
        </div>
      </div>
    </div>
  )
}

function DeployModal({ fn, onClose, onSubmit }) {
  const [form, setForm] = useState({
    tenant_id: '', cluster: '', region: '', cloud: '', use_spot: false,
  })

  const set = (key, val) => setForm(f => ({ ...f, [key]: val }))

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={e => e.stopPropagation()}>
        <div className="modal-title">Deploy: {fn.name}</div>
        <div style={{ fontSize: 13, color: 'var(--text-dim)', marginBottom: 20 }}>
          Create a new instance of this function on a cluster.
        </div>

        <div className="form-group">
          <label className="form-label">Tenant ID</label>
          <input className="form-input" placeholder="hospital-a"
            value={form.tenant_id} onChange={e => set('tenant_id', e.target.value)} />
        </div>

        <div className="form-row">
          <div className="form-group">
            <label className="form-label">Cloud (optional)</label>
            <select className="form-select" value={form.cloud}
              onChange={e => set('cloud', e.target.value)}>
              <option value="">Auto (cheapest)</option>
              <option value="aws">AWS</option>
              <option value="gcp">GCP</option>
              <option value="azure">Azure</option>
              <option value="kubernetes">Kubernetes (on-prem)</option>
            </select>
          </div>
          <div className="form-group">
            <label className="form-label">Region (optional)</label>
            <input className="form-input" placeholder="us-east-1 or auto"
              value={form.region} onChange={e => set('region', e.target.value)} />
          </div>
        </div>

        <div className="form-group">
          <label className="form-label">Cluster (optional)</label>
          <input className="form-input" placeholder="Leave empty for SkyPilot auto-select"
            value={form.cluster} onChange={e => set('cluster', e.target.value)} />
        </div>

        <div className="form-group">
          <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer' }}>
            <input type="checkbox" checked={form.use_spot}
              onChange={e => set('use_spot', e.target.checked)} />
            <span style={{ fontSize: 13 }}>Use spot/preemptible instances (3-6x cheaper)</span>
          </label>
        </div>

        <div className="form-actions">
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" onClick={() => onSubmit(form)}
            disabled={!form.tenant_id}>Deploy</button>
        </div>
      </div>
    </div>
  )
}

function ClustersPage() {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    const fetchClusters = async () => {
      try {
        const res = await fetch('/api/clusters')
        const json = await res.json()
        setData(json)
      } catch {
        setData(null)
      }
      setLoading(false)
    }
    fetchClusters()
    const interval = setInterval(fetchClusters, 5000)
    return () => clearInterval(interval)
  }, [])

  if (loading) return <div className="empty-state"><div className="empty-icon">...</div><div>Loading cluster data...</div></div>

  if (!data || data.error) return (
    <div className="empty-state">
      <div className="empty-icon">!</div>
      <div>Could not connect to control plane. Make sure the cluster API is running.</div>
    </div>
  )

  const { summary, clusters } = data

  return (
    <>
      <div className="page-header">
        <div>
          <h2>Clusters</h2>
          <p className="subtitle">GPU infrastructure overview — {summary.cluster_count} cluster{summary.cluster_count !== 1 ? 's' : ''} connected</p>
        </div>
      </div>

      <div className="cluster-overview">
        <div className="cluster-stats" style={{ marginBottom: 24 }}>
          <div className="stat-card">
            <div className="stat-value">{summary.cluster_count}</div>
            <div className="stat-label">Clusters</div>
          </div>
          <div className="stat-card">
            <div className="stat-value">{summary.total_gpus}</div>
            <div className="stat-label">Total GPUs</div>
          </div>
          <div className="stat-card">
            <div className="stat-value stat-green">{summary.available_gpus}</div>
            <div className="stat-label">Available</div>
          </div>
          <div className="stat-card">
            <div className="stat-value">{summary.total_gpus - summary.available_gpus}</div>
            <div className="stat-label">In Use</div>
          </div>
          <div className="stat-card">
            <div className="stat-value">{summary.total_nodes}</div>
            <div className="stat-label">Total Nodes</div>
          </div>
        </div>

        {clusters.map(cluster => {
          const gpuTypes = cluster.gpu_types || []
          const nodes = cluster.nodes || []
          const gpuNodeCount = nodes.filter(n => n.gpu_count > 0).length

          return (
            <div key={cluster.cluster_id} className="cluster-header-card" style={{ marginBottom: 20 }}>
              <div className="cluster-header-top">
                <div>
                  <h3>{cluster.cluster_name || cluster.cluster_id}</h3>
                  <span className="subtitle">{cluster.provider} &middot; {cluster.k8s_version} &middot; {nodes.length} nodes &middot; {gpuNodeCount} GPU nodes</span>
                </div>
                <span className={`status-badge status-${cluster.status}`}>{cluster.status}</span>
              </div>

              <div className="cluster-stats" style={{ marginBottom: 16 }}>
                <div className="stat-card">
                  <div className="stat-value">{cluster.total_gpus}</div>
                  <div className="stat-label">GPUs</div>
                </div>
                <div className="stat-card">
                  <div className="stat-value stat-green">{cluster.available_gpus}</div>
                  <div className="stat-label">Available</div>
                </div>
                <div className="stat-card">
                  <div className="stat-value">{cluster.total_gpus - cluster.available_gpus}</div>
                  <div className="stat-label">In Use</div>
                </div>
              </div>

              {gpuTypes.length > 0 && (
                <>
                  <h4 className="section-title" style={{ marginBottom: 8 }}>GPU Types</h4>
                  <div className="gpu-types-grid" style={{ marginBottom: 16 }}>
                    {gpuTypes.map(gpu => (
                      <div key={gpu.name} className="gpu-type-card">
                        <div className="gpu-type-name">{(gpu.name || '').replace('NVIDIA-', '').replace('SXM4-', '')}</div>
                        <div className="gpu-type-details">
                          <span className="badge badge-gpu">{gpu.count} GPUs</span>
                          <span className="badge">{gpu.family}</span>
                          <span className="badge badge-available">{gpu.available} available</span>
                        </div>
                        <div className="gpu-bar">
                          <div className="gpu-bar-fill" style={{ width: `${gpu.count > 0 ? ((gpu.count - gpu.available) / gpu.count) * 100 : 0}%` }}></div>
                        </div>
                        <div className="gpu-bar-label">{gpu.count - gpu.available} / {gpu.count} in use</div>
                      </div>
                    ))}
                  </div>
                </>
              )}

              <h4 className="section-title" style={{ marginBottom: 8 }}>Nodes</h4>
              <div className="nodes-list">
                {nodes.map(node => (
                  <div key={node.name} className={`node-card ${node.gpu_count > 0 ? 'node-gpu' : ''}`}>
                    <div className="node-header">
                      <div className="node-info">
                        <span className={`status-dot ${node.status === 'Ready' ? 'status-dot-green' : 'status-dot-red'}`}></span>
                        <strong>{node.name}</strong>
                        <span className="node-role">{node.role}</span>
                      </div>
                      {node.gpu_count > 0 && (
                        <div className="node-gpu-badges">
                          <span className="badge badge-gpu">{node.gpu_count}x GPU</span>
                          <span className="badge">{(node.gpu_type || '').replace('NVIDIA-', '')}</span>
                          <span className="badge">{node.gpu_family}</span>
                        </div>
                      )}
                    </div>
                    <div className="node-details">
                      <span>CPU: {node.cpu}</span>
                      <span>Memory: {node.memory_gb} GB</span>
                      <span>{node.k8s_version}</span>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )
        })}
      </div>
    </>
  )
}

export default App
