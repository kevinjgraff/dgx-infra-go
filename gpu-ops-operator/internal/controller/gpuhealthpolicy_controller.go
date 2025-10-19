/*
Copyright 2025 Kevin Graff.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//apierrors "k8s.io/apimachinery/pkg/api/errors"
	//"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	//"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opsv1 "github.com/kevinjgraff/dgx-infra-go/api/v1"
)

// GPUHealthPolicyReconciler reconciles a GPUHealthPolicy object.
type GPUHealthPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// RBAC markers (controller-gen reads these to augment role.yaml if needed)
// +kubebuilder:rbac:groups=ops.example.com,resources=gpuhealthpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ops.example.com,resources=gpuhealthpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ops.example.com,resources=gpuhealthpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=evictions,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update


func (r *GPUHealthPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("ghp", req.NamespacedName)

	// Fetch the policy (exit cleanly if deleted)
	var policy opsv1.GPUHealthPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// List nodes to evaluate
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		logger.Error(err, "list nodes failed")
		return ctrl.Result{}, err
	}

	// Scrape metric endpoint -> map[lower(hostname)]count
	errCounts, scrapeErr := scrapeGPUErrorCounts(policy.Spec.MetricURL)
	if scrapeErr != nil {
		logger.Error(scrapeErr, "metric scrape failed", "metricURL", policy.Spec.MetricURL)
		// Try again soon; external systems can be flaky
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Compute which hostnames are unhealthy given the threshold
	unhealthy := sets.New[string]()
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		hn := n.Labels["kubernetes.io/hostname"]
		if hn == "" {
			continue // node without hostname label 
		}
		if count := errCounts[strings.ToLower(hn)]; count > policy.Spec.Threshold {
			unhealthy.Insert(hn)
		}
	}

	// Cooldown between remediations
	now := time.Now()
	if policy.Status.LastRemediation != nil && policy.Spec.CooldownSeconds > 0 {
		next := policy.Status.LastRemediation.Time.Add(time.Duration(policy.Spec.CooldownSeconds) * time.Second)
		if now.Before(next) {
			return ctrl.Result{RequeueAfter: time.Until(next)}, nil
		}
	}

	// Apply actions (taint; evictions currently disabled))
	taintKey := policy.Spec.TaintKey
	var touched []string

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		hn := node.Labels["kubernetes.io/hostname"]
		if hn == "" {
			continue
		}
		isUnhealthy := unhealthy.Has(hn)

		// Ensure taint presence/absence reflects health
		changed, err := r.ensureTaint(ctx, node, taintKey, isUnhealthy, policy.Spec.DryRun)
		if err != nil {
			logger.Error(err, "ensure taint failed", "node", node.Name, "unhealthy", isUnhealthy)
			continue
		}
		if changed {
			touched = append(touched, node.Name)
			if policy.Spec.DryRun {
				logger.Info("DRY-RUN: would update taint", "node", node.Name, "unhealthy", isUnhealthy)
			} else {
				logger.Info("updated taint", "node", node.Name, "unhealthy", isUnhealthy)
			}
		}

		// Evictions intentionally disabled until taints verified to persist
		// (Re-enable with a safe subresource helper once ready)
		// if isUnhealthy && selector != nil {
		//     if err := r.evictMatchingPods(ctx, node.Name, selector, policy.Spec.DryRun); err != nil {
		//         logger.Error(err, "eviction pass failed", "node", node.Name)
		//     }
		// }
	}

	// Patch-based status update (status is a value type)
	{
		polCopy := policy.DeepCopy()

		// Build a deterministic, sorted list of unhealthy nodes
		list := unhealthy.UnsortedList()
		sort.Strings(list)

		polCopy.Status.UnhealthyNodes = list
		t := metav1.NewTime(now)
		polCopy.Status.LastRemediation = &t

		if err := r.Status().Patch(ctx, polCopy, client.MergeFrom(&policy)); err != nil {
			logger.Error(err, "status patch failed")
			return ctrl.Result{}, err
		}

		// Emit a visibility event (guard recorder)
		if len(touched) > 0 {
			evt := fmt.Sprintf("Remediation touched nodes: %v (dryRun=%v)", strings.Join(touched, ","), policy.Spec.DryRun)
			if r.Recorder != nil {
				r.Recorder.Event(&policy, corev1.EventTypeNormal, "Remediation", evt)
			} else {
				logger.Info("event recorder is nil; skipping Event()", "event", evt)
			}
		}
	}


	// 8) Requeue periodically to rescan metrics
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}


func (r *GPUHealthPolicyReconciler) ensureTaint(ctx context.Context, node *corev1.Node, key string, wantUnhealthy bool, dryRun bool) (bool, error) {
	// Always work on a fresh copy from the API server so ResourceVersion is current
	var fresh corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node.Name}, &fresh); err != nil {
		return false, err
	}

	taint := corev1.Taint{
		Key:    key,
		Value:  "true",
		Effect: corev1.TaintEffectNoSchedule,
	}

	has := func(n *corev1.Node) bool {
		for _, t := range n.Spec.Taints {
			if t.Key == taint.Key && t.Effect == taint.Effect {
				return true
			}
		}
		return false
	}

	orig := fresh.DeepCopy()
	changed := false

	if wantUnhealthy && !has(&fresh) {
		if !dryRun {
			fresh.Spec.Taints = append(fresh.Spec.Taints, taint)
			if err := r.Patch(ctx, &fresh, client.MergeFrom(orig)); err != nil {
				return false, err
			}
		}
		changed = true
	}

	if !wantUnhealthy && has(&fresh) {
		if !dryRun {
			newTaints := make([]corev1.Taint, 0, len(fresh.Spec.Taints))
			for _, t := range fresh.Spec.Taints {
				if !(t.Key == taint.Key && t.Effect == taint.Effect) {
					newTaints = append(newTaints, t)
				}
			}
			fresh.Spec.Taints = newTaints
			if err := r.Patch(ctx, &fresh, client.MergeFrom(orig)); err != nil {
				return false, err
			}
		}
		changed = true
	}

	return changed, nil
}

func (r *GPUHealthPolicyReconciler) evictMatchingPods(ctx context.Context, nodeName string, selector labels.Selector, dryRun bool) error {
	// List pods on the node.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// if field index not set, fall back to full list + in-memory filter
		if err := r.List(ctx, &pods); err != nil {
			return err
		}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != nodeName {
			continue
		}
		if !selector.Matches(labels.Set(p.Labels)) {
			continue
		}
		// Skip mirror/static pods and kube-system criticals.
		if isMirrorPod(p) || p.Namespace == "kube-system" {
			continue
		}
		ev := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.Name,
				Namespace: p.Namespace,
			},
			DeleteOptions: &metav1.DeleteOptions{},
		}
		if dryRun {
			continue
		}
		// Try eviction, controller will retry later if it fails.
		_ = r.SubResource("eviction").Create(ctx, p, ev)
	}
	return nil
}

func isMirrorPod(p *corev1.Pod) bool {
	_, has := p.Annotations[corev1.MirrorPodAnnotationKey]
	return has
}

// scrapeGPUErrorCounts reads a Prometheus text exposition and extracts
// a metric named gpu_xid_errors_total labeled by instance/hostname.
// Returns a map[lowerCaseHost]count.
func scrapeGPUErrorCounts(url string) (map[string]int, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	b, _ := io.ReadAll(resp.Body)
	lines := strings.Split(string(b), "\n")

	// gpu_xid_errors_total{instance="dev-control-plane",job="gpuhealth"} 5
	re := regexp.MustCompile(`^gpu_xid_errors_total\{[^}]*instance="([^"]+)"[^}]*\}\s+([0-9]+)$`)

	out := map[string]int{}
	for _, ln := range lines {
		m := re.FindStringSubmatch(strings.TrimSpace(ln))
		if len(m) != 3 {
			continue
		}
		host := strings.ToLower(m[1])
		var val int
		fmt.Sscanf(m[2], "%d", &val)
		out[host] = val
	}
	return out, nil
}

func (r *GPUHealthPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// add a field index for pod.spec.nodeName for efficient listing
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		p := o.(*corev1.Pod)
		if p.Spec.NodeName == "" {
			return nil
		}
		return []string{p.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&opsv1.GPUHealthPolicy{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

