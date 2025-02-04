// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudsqlapi "github.com/GoogleCloudPlatform/cloud-sql-proxy-operator/internal/api/v1alpha1"
	"github.com/GoogleCloudPlatform/cloud-sql-proxy-operator/internal/workload"
	"github.com/go-logr/logr"
)

const finalizerName = cloudsqlapi.AnnotationPrefix + "/AuthProxyWorkload-finalizer"

var (
	requeueNow       = ctrl.Result{Requeue: true}
	requeueWithDelay = ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}
)

type recentlyDeletedCache struct {
	lock   sync.RWMutex
	values map[types.NamespacedName]bool
}

func (c *recentlyDeletedCache) set(k types.NamespacedName, deleted bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.values == nil {
		c.values = map[types.NamespacedName]bool{}
	}
	c.values[k] = deleted
}

func (c *recentlyDeletedCache) get(k types.NamespacedName) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	deleted, ok := c.values[k]
	if !ok {
		return false
	}
	return deleted
}

// AuthProxyWorkloadReconciler reconciles a AuthProxyWorkload object
type AuthProxyWorkloadReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	recentlyDeleted *recentlyDeletedCache
	updater         *workload.Updater
}

// NewAuthProxyWorkloadManager constructs an AuthProxyWorkloadReconciler
func NewAuthProxyWorkloadReconciler(mgr ctrl.Manager, u *workload.Updater) (*AuthProxyWorkloadReconciler, error) {
	r := &AuthProxyWorkloadReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		recentlyDeleted: &recentlyDeletedCache{},
		updater:         u,
	}
	err := r.SetupWithManager(mgr)
	return r, err
}

// SetupWithManager adds this AuthProxyWorkload controller to the controller-runtime
// manager.
func (r *AuthProxyWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudsqlapi.AuthProxyWorkload{}).
		Complete(r)
}

//+kubebuilder:rbac:groups=apps,resources=*,verbs=get;list;watch
//+kubebuilder:rbac:groups=batch,resources=*,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=*,verbs=get;list;watch

//+kubebuilder:rbac:groups=cloudsql.cloud.google.com,resources=authproxyworkloads,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudsql.cloud.google.com,resources=authproxyworkloads/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudsql.cloud.google.com,resources=authproxyworkloads/finalizers,verbs=update

//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete

// Reconcile updates the state of the cluster so that AuthProxyWorkload instances
// have their configuration reflected correctly on workload PodSpec configuration.
// This reconcile loop runs when an AuthProxyWorkload is added, modified or deleted.
// It updates annotations on matching workloads indicating those workload that
// need to be updated.
//
// As this controller's Reconcile() function patches the annotations on workloads,
// the PodAdmissionWebhook.Handle() method is called by k8s api, which is
// where the PodSpec is modified to match the AuthProxyWorkload configuration.
//
// This function can only make one update to the AuthProxyWorkload per loop, so it
// is written like a state machine. It will quickly do a single update, often to
// the status, and then return. Sometimes it instructs the controller runtime to quickly
// requeue another call to Reconcile, so that it can further process the
// AuthProxyWorkload. It often takes several calls to Reconcile() to finish the
// reconcilliation of a single change to an AuthProxyWorkload.
//
// For more details, check Reconcile and its Result here:
// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.1/pkg/reconcile
func (r *AuthProxyWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	var err error

	resource := &cloudsqlapi.AuthProxyWorkload{}

	l.Info("Reconcile loop started AuthProxyWorkload", "name", req.NamespacedName)
	if err = r.Get(ctx, req.NamespacedName, resource); err != nil {
		// The resource can't be loaded.
		// If it was recently deleted, then ignore the error and don't requeue.
		if r.recentlyDeleted.get(req.NamespacedName) {
			return ctrl.Result{}, nil
		}

		// otherwise, report the error and requeue. This is likely caused by a delay
		// in reaching consistency in the eventually-consistent kubernetes API.
		l.Error(err, "unable to fetch resource")
		return requeueWithDelay, err
	}

	// If this was deleted, doDelete()
	// DeletionTimestamp metadata field is set by k8s when a resource
	// has been deleted but the finalizers are still present. We check that this
	// value is not zero To determine when a resource is deleted and waiting for
	// completion of finalizers.
	if !resource.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("Reconcile delete for AuthProxyWorkload",
			"name", resource.GetName(),
			"namespace", resource.GetNamespace(),
			"gen", resource.GetGeneration())
		r.recentlyDeleted.set(req.NamespacedName, true)
		// the object has been deleted
		return r.doDelete(ctx, resource, l)
	}

	l.Info("Reconcile add/update for AuthProxyWorkload",
		"name", resource.GetName(),
		"namespace", resource.GetNamespace(),
		"gen", resource.GetGeneration())
	r.recentlyDeleted.set(req.NamespacedName, false)
	return r.doCreateUpdate(ctx, l, resource)
}

// doDelete removes our finalizer and updates the related workloads
// when the reconcile loop receives an AuthProxyWorkload that was deleted.
func (r *AuthProxyWorkloadReconciler) doDelete(ctx context.Context, resource *cloudsqlapi.AuthProxyWorkload, l logr.Logger) (ctrl.Result, error) {

	// Mark all related workloads as needing to be updated
	_, err := r.updateWorkloadStatus(ctx, l, resource)
	if err != nil {
		return requeueNow, err
	}

	// Remove the finalizer so that the object can be fully deleted
	if controllerutil.ContainsFinalizer(resource, finalizerName) {
		controllerutil.RemoveFinalizer(resource, finalizerName)
		err = r.Update(ctx, resource)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// doCreateUpdate reconciles an AuthProxyWorkload resource that has been created
// or updated, making sure that related workloads get updated.
//
// This is implemented as a state machine. The current state is determined using
// - the absence or presence of this controller's finalizer
// - the success or error when retrieving workloads related to this resource
// - the number of workloads needing updates
// - the condition `UpToDate` status and reason
//
// States:
// |  state  | finalizer| fetch err | len(wl)      | Name                      |
// |---------|----------|-----------|--------------|---------------------      |
// | 0       | *        | *         | *            | start                     |
// | 1.1     | absent   | *         | *            | needs finalizer           |
// | 1.2     | present  | error     | *            | can't list workloads      |
// | 2.1     | present  | nil       | == 0         | no workloads to reconcile |
// | 3.1     | present  | nil       | > 0          | workloads reconciled      |
//
//	start ---x
//	          \---> 1.1 --> (requeue, goto start)
//	           \---> 1.2 --> (requeue, goto start)
//	            \---> 2.1 --> (end)
//	             \---> 3.1 --> (end)
func (r *AuthProxyWorkloadReconciler) doCreateUpdate(ctx context.Context, l logr.Logger, resource *cloudsqlapi.AuthProxyWorkload) (ctrl.Result, error) {
	orig := resource.DeepCopy()
	var err error
	// State 0: The reconcile loop for a single AuthProxyWorkload resource begins
	// when an AuthProxyWorkload resource is created, modified, or deleted in the k8s api
	// or when that AuthProxyWorkload resource is requeued for another reconcile loop.

	if !controllerutil.ContainsFinalizer(resource, finalizerName) {
		// State 1.1: This is a brand new thing that doesn't have a finalizer.
		// Add the finalizer and requeue for another run through the reconcile loop
		return r.applyFinalizer(ctx, l, resource)
	}

	// find all workloads that relate to this AuthProxyWorkload resource
	allWorkloads, err := r.updateWorkloadStatus(ctx, l, resource)
	if err != nil {
		// State 1.2 - unable to read workloads, abort and try again after a delay.
		return requeueWithDelay, err
	}

	// State 2: If workload reconcile has not yet started, then start it.

	// State 2.1: When there are no workloads, then mark this as "UpToDate" true,
	// do not requeue.
	if len(allWorkloads) == 0 {
		return r.reconcileResult(ctx, l, resource, orig, cloudsqlapi.ReasonNoWorkloadsFound, "No workload updates needed")
	}

	// State 3.1: Workload updates are in progress. Check if the workload updates
	// are complete.
	//
	message := fmt.Sprintf("Reconciled %d matching workloads complete", len(allWorkloads))

	return r.reconcileResult(ctx, l, resource, orig, cloudsqlapi.ReasonFinishedReconcile, message)
}

// workloadsReconciled  State 3.1: If workloads are all up to date, mark the condition
// "UpToDate" true and do not requeue.
func (r *AuthProxyWorkloadReconciler) reconcileResult(ctx context.Context, l logr.Logger, resource *cloudsqlapi.AuthProxyWorkload, orig *cloudsqlapi.AuthProxyWorkload, reason, message string) (ctrl.Result, error) {

	// Workload updates are complete, update the status
	resource.Status.Conditions = replaceCondition(resource.Status.Conditions, &metav1.Condition{
		Type:               cloudsqlapi.ConditionUpToDate,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: resource.GetGeneration(),
		Reason:             reason,
		Message:            message,
	})
	err := r.patchAuthProxyWorkloadStatus(ctx, resource, orig)
	if err != nil {
		l.Error(err, "Unable to patch status before beginning workloads", "AuthProxyWorkload", resource.GetNamespace()+"/"+resource.GetName())
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// applyFinalizer adds the finalizer so that the operator is notified when
// this AuthProxyWorkload resource gets deleted. applyFinalizer is called only
// once, when the resource first added.
func (r *AuthProxyWorkloadReconciler) applyFinalizer(
	ctx context.Context, l logr.Logger, resource *cloudsqlapi.AuthProxyWorkload) (ctrl.Result, error) {

	// The AuthProxyWorkload resource needs a finalizer, so add
	// the finalizer, exit the reconcile loop and requeue.
	controllerutil.AddFinalizer(resource, finalizerName)

	err := r.Update(ctx, resource)
	if err != nil {
		l.Info("Error adding finalizer. Will requeue for reconcile.", "err", err)
		return requeueNow, err
	}

	l.Info("Added finalizer. Will requeue quickly for reconcile", "err", err)
	return requeueNow, err
}

// patchAuthProxyWorkloadStatus uses the PATCH method to incrementally update
// the AuthProxyWorkload.Status field.
func (r *AuthProxyWorkloadReconciler) patchAuthProxyWorkloadStatus(
	ctx context.Context, resource *cloudsqlapi.AuthProxyWorkload, orig *cloudsqlapi.AuthProxyWorkload) error {
	err := r.Client.Status().Patch(ctx, resource, client.MergeFrom(orig))
	if err != nil {
		return err
	}
	err = r.Get(ctx, types.NamespacedName{
		Namespace: resource.GetNamespace(),
		Name:      resource.GetName(),
	}, orig)
	return err
}

// updateWorkloadStatus lists all workloads related to a cloudsql instance and
// updates the needs update annotations using internal.UpdateWorkloadAnnotation.
// Once the workload is saved, the workload admission mutate webhook will
// apply the correct containers to this instance.
func (r *AuthProxyWorkloadReconciler) updateWorkloadStatus(ctx context.Context, _ logr.Logger, resource *cloudsqlapi.AuthProxyWorkload) (matching []workload.Workload, retErr error) {

	matching, err := r.listWorkloads(ctx, resource.Spec.Workload, resource.GetNamespace())
	if err != nil {
		return nil, err
	}

	// all matching workloads get a new annotation that will be removed
	// when the reconcile loop for outOfDate is completed.
	for _, wl := range matching {
		s := newStatus(wl)
		s.Conditions = replaceCondition(s.Conditions, &metav1.Condition{
			Type:               cloudsqlapi.ConditionWorkloadUpToDate,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: resource.GetGeneration(),
			Reason:             cloudsqlapi.ReasonUpToDate,
			Message:            "No update needed for this workload",
		})
		resource.Status.WorkloadStatus = replaceStatus(resource.Status.WorkloadStatus, s)
	}

	return matching, nil
}

// replaceStatus replace a status with the same name, namespace, kind, and version,
// or appends updatedStatus to statuses
func replaceStatus(statuses []*cloudsqlapi.WorkloadStatus, updatedStatus *cloudsqlapi.WorkloadStatus) []*cloudsqlapi.WorkloadStatus {

	var updated bool
	for i := range statuses {
		s := statuses[i]
		if s.Name == updatedStatus.Name &&
			s.Namespace == updatedStatus.Namespace &&
			s.Kind == updatedStatus.Kind &&
			s.Version == updatedStatus.Version {
			statuses[i] = updatedStatus
			updated = true
		}
	}
	if !updated {
		statuses = append(statuses, updatedStatus)
	}
	return statuses
}

func findCondition(conds []*metav1.Condition, name string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == name {
			return conds[i]
		}
	}
	return nil
}

// replaceCondition replace a status with the same name, namespace, kind, and version,
// or appends updatedStatus to statuses
func replaceCondition(conds []*metav1.Condition, newC *metav1.Condition) []*metav1.Condition {
	for i := range conds {
		c := conds[i]
		if c.Type != newC.Type {
			continue
		}

		if conds[i].Status == newC.Status && !conds[i].LastTransitionTime.IsZero() {
			newC.LastTransitionTime = conds[i].LastTransitionTime
		} else {
			newC.LastTransitionTime = metav1.NewTime(time.Now())
		}
		conds[i] = newC
		return conds
	}

	newC.LastTransitionTime = metav1.NewTime(time.Now())
	conds = append(conds, newC)
	return conds
}

// newStatus creates a WorkloadStatus from a workload with identifying
// fields filled in.
func newStatus(wl workload.Workload) *cloudsqlapi.WorkloadStatus {
	return &cloudsqlapi.WorkloadStatus{
		Kind:      wl.Object().GetObjectKind().GroupVersionKind().Kind,
		Version:   wl.Object().GetObjectKind().GroupVersionKind().GroupVersion().Identifier(),
		Namespace: wl.Object().GetNamespace(),
		Name:      wl.Object().GetName(),
	}
}

// listWorkloads produces a list of Workload's that match the WorkloadSelectorSpec
// in the specified namespace.
func (r *AuthProxyWorkloadReconciler) listWorkloads(ctx context.Context, workloadSelector cloudsqlapi.WorkloadSelectorSpec, ns string) ([]workload.Workload, error) {
	if workloadSelector.Namespace != "" {
		ns = workloadSelector.Namespace
	}

	if workloadSelector.Name != "" {
		return r.loadByName(ctx, workloadSelector, ns)
	}

	return r.loadByLabelSelector(ctx, workloadSelector, ns)
}

// loadByName loads a single workload by name.
func (r *AuthProxyWorkloadReconciler) loadByName(ctx context.Context, workloadSelector cloudsqlapi.WorkloadSelectorSpec, ns string) ([]workload.Workload, error) {
	var wl workload.Workload

	key := client.ObjectKey{Namespace: ns, Name: workloadSelector.Name}

	wl, err := workload.WorkloadForKind(workloadSelector.Kind)
	if err != nil {
		return nil, fmt.Errorf("unable to load by name %s/%s:  %v", key.Namespace, key.Name, err)
	}

	err = r.Get(ctx, key, wl.Object())
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil // empty list when no named workload is found. It is not an error.
		}
		return nil, fmt.Errorf("unable to load resource by name %s/%s:  %v", key.Namespace, key.Name, err)
	}

	return []workload.Workload{wl}, nil
}

// loadByLabelSelector loads workloads matching a label selector
func (r *AuthProxyWorkloadReconciler) loadByLabelSelector(ctx context.Context, workloadSelector cloudsqlapi.WorkloadSelectorSpec, ns string) ([]workload.Workload, error) {
	l := log.FromContext(ctx)

	sel, err := workloadSelector.LabelsSelector()

	if err != nil {
		return nil, err
	}
	_, gk := schema.ParseKindArg(workloadSelector.Kind)
	wl, err := workload.WorkloadListForKind(gk.Kind)
	if err != nil {
		return nil, err
	}
	err = r.List(ctx, wl.List(), client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel})
	if err != nil {
		l.Error(err, "Unable to list s for workloadSelector", "selector", sel)
		return nil, err
	}
	return wl.Workloads(), nil

}
