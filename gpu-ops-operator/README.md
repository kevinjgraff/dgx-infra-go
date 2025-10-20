# gpu-ops-operator

The **GPU Ops Operator** is a Kubernetes controller designed to automatically monitor GPU health across nodes and apply remediation policies. It provides an automated feedback loop that reads GPU error metrics, identifies unhealthy nodes, taints them to prevent new workloads, and optionally evicts existing GPU workloads for recovery or maintenance.

## Description

This operator simplifies GPU fleet management in Kubernetes clusters by continuously evaluating GPU health data (such as NVIDIA XID errors) from a metrics endpoint like Prometheus Pushgateway.  
It uses custom resources (`GPUHealthPolicy`) to define how GPU nodes should be evaluated and remediated based on error thresholds, cooldown timers, and labels.  

**Core capabilities:**
- Scrapes GPU error metrics (e.g., `gpu_xid_errors_total`) from Pushgateway.
- Dynamically taints unhealthy nodes with a configurable key (e.g., `gpu-unhealthy=true:NoSchedule`).
- Clears taints when the node’s metrics return to normal.
- Optionally evicts pods matching a label selector on unhealthy nodes.
- Emits Kubernetes events and updates CR status for visibility.
- Supports `dryRun` and `cooldownSeconds` for controlled operation.

This enables proactive detection and isolation of failing GPUs in clusters—preventing job disruption and improving scheduling reliability.

## Getting Started

### Prerequisites
- Go **v1.24.0+**
- Docker **17.03+**
- kubectl **v1.11.3+**
- A running Kubernetes **v1.11.3+** cluster
- Optional: [Prometheus Pushgateway](https://github.com/prometheus/pushgateway) deployed in a namespace (default: `observability`)

### Deploy to Cluster

**1. Build and push the operator image:**
```bash
make docker-build docker-push IMG=ghcr.io/<your-username>/gpu-ops-operator:dev
```

**2. Install CRDs:**
```bash
make install
```

**3. Deploy the controller:**
```bash
make deploy IMG=ghcr.io/<your-username>/gpu-ops-operator:dev
```

> If you encounter RBAC errors, ensure you have cluster-admin privileges or are operating under an admin context.

### Example Usage

**Create a GPUHealthPolicy resource:**
```yaml
apiVersion: ops.example.com/v1
kind: GPUHealthPolicy
metadata:
  name: default-policy
  namespace: default
spec:
  metricURL: "http://pushgateway.observability.svc.cluster.local:9091/metrics"
  threshold: 0
  taintKey: "gpu-unhealthy"
  cooldownSeconds: 30
  dryRun: false
  labelSelector: "app=gpu-workload"
```

Apply it:
```bash
kubectl apply -f config/samples/ops_v1_gpuhealthpolicy.yaml
```

Verify status and taints:
```bash
kubectl get gpuhealthpolicy default-policy -o yaml
kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints
```

Trigger a reconcile (for testing):
```bash
kubectl annotate gpuhealthpolicy default-policy "ops.example.com/reconcileAt=$(date +%s)" --overwrite
```

### Uninstall

**Delete all GPUHealthPolicy instances:**
```bash
kubectl delete -k config/samples/
```

**Remove CRDs:**
```bash
make uninstall
```

**Remove the operator deployment:**
```bash
make undeploy
```

## Building Distributions

### Single YAML Installer

Generate an all-in-one manifest:
```bash
make build-installer IMG=ghcr.io/<your-username>/gpu-ops-operator:dev
```

Then install:
```bash
kubectl apply -f dist/install.yaml
```

### Helm Chart (optional)

Generate a Helm chart:
```bash
kubebuilder edit --plugins=helm/v1-alpha
```

A chart will be available under `dist/chart/`, which can be deployed using Helm.

## Contributing

Contributions are welcome!  
You can:
1. Open issues for feature requests or bug reports.
2. Fork and create pull requests for improvements.
3. Use `make help` to discover supported build and deployment targets.

Ensure your changes pass `make test` and `go fmt ./...` before committing.

## License

Copyright © 2025 **Kevin Graff**

Licensed under the Apache License, Version 2.0.  
You may obtain a copy of the License at:

```
http://www.apache.org/licenses/LICENSE-2.0
```

Unless required by applicable law or agreed to in writing, software distributed under this License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND.
