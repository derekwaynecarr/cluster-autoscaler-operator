package machineautoscaler

import (
	"context"
	"errors"

	"github.com/golang/glog"
	autoscalingv1alpha1 "github.com/openshift/cluster-autoscaler-operator/pkg/apis/autoscaling/v1alpha1"
	"github.com/openshift/cluster-autoscaler-operator/pkg/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// MachineTargetFinalizer is the finalizer added to MachineAutoscaler
	// instances to allow for cleanup of annotations on target resources.
	MachineTargetFinalizer = "machinetarget.autoscaling.openshift.io"

	// MachineTargetOwnerAnnotation is the annotation used to mark a
	// target resource's autoscaling as owned by a MachineAutoscaler.
	MachineTargetOwnerAnnotation = "autoscaling.openshift.io/machineautoscaler"

	minSizeAnnotation = "sigs.k8s.io/cluster-api-autoscaler-node-group-min-size"
	maxSizeAnnotation = "sigs.k8s.io/cluster-api-autoscaler-node-group-max-size"
)

// ErrUnsupportedTarget is the error returned when a target references an object
// with an unsupported GroupVersionKind.
var ErrUnsupportedTarget = errors.New("unsupported MachineAutoscaler target")

// SupportedTargetGVKs is the list of GroupVersionKinds supported as targets for
// a MachineAutocaler instance.
var SupportedTargetGVKs = []schema.GroupVersionKind{
	{Group: "cluster.k8s.io", Version: "v1alpha1", Kind: "MachineSet"},
	{Group: "cluster.k8s.io", Version: "v1alpha1", Kind: "MachineDeployment"},
}

// NewReconciler returns a new Reconciler.
func NewReconciler(mgr manager.Manager) *Reconciler {
	return &Reconciler{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
	}
}

// AddToManager adds a new Controller to mgr with r as the reconcile.Reconciler
func (r *Reconciler) AddToManager(mgr manager.Manager) error {
	// Create a new controller
	c, err := controller.New("machineautoscaler-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource MachineAutoscaler
	err = c.Watch(&source.Kind{Type: &autoscalingv1alpha1.MachineAutoscaler{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to each supported target resource type and enqueue
	// reconcile requests for their owning MachineAutoscaler resources.
	for _, gvk := range SupportedTargetGVKs {
		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(gvk)

		err := c.Watch(
			&source.Kind{Type: target},
			&handler.EnqueueRequestsFromMapFunc{
				ToRequests: handler.ToRequestsFunc(targetOwnerRequest),
			})

		if err != nil {
			return err
		}
	}

	return nil
}

var _ reconcile.Reconciler = &Reconciler{}

// Reconciler reconciles a MachineAutoscaler object
type Reconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a MachineAutoscaler object and
// makes changes based on the state read and what is in the
// MachineAutoscaler.Spec
func (r *Reconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	glog.Infof("Reconciling MachineAutoscaler %s/%s\n", request.Namespace, request.Name)

	// Fetch the MachineAutoscaler instance
	ma := &autoscalingv1alpha1.MachineAutoscaler{}
	err := r.client.Get(context.TODO(), request.NamespacedName, ma)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile
			// request.  Owned objects are automatically garbage collected. For
			// additional cleanup logic use finalizers.
			// Return and don't requeue.
			return reconcile.Result{}, nil
		}

		// Error reading the object - requeue the request.
		glog.Errorf("Error reading MachineAutoscaler: %v", err)
		return reconcile.Result{}, err
	}

	target, err := r.GetTarget(ma)
	if err != nil {
		glog.Errorf("Error getting target: %v", err)
		return reconcile.Result{}, err
	}

	// Set the MachineAutoscaler as the owner of the target.
	ownerModifed, err := target.SetOwner(ma)
	if err != nil {
		glog.Errorf("Error setting target owner: %v", err)
		return reconcile.Result{}, err
	}

	// If the owner is newly added, remove any existing limits.
	// This will force an update to bring things into sync.
	if ownerModifed {
		target.RemoveLimits()
	}

	// Handle MachineAutoscaler deletion.
	if ma.GetDeletionTimestamp() != nil {
		if err := r.FinalizeTarget(target); err != nil {
			glog.Errorf("Error finalizing target: %v", err)
			return reconcile.Result{}, err
		}

		if err := r.RemoveFinalizer(ma); err != nil {
			glog.Errorf("Error removing finalizer: %v", err)
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	// Ensure our finalizers have been added.
	if err := r.EnsureFinalizer(ma); err != nil {
		glog.Errorf("Error setting finalizer: %v", err)
		return reconcile.Result{}, err
	}

	min := int(ma.Spec.MinReplicas)
	max := int(ma.Spec.MaxReplicas)

	if err := r.UpdateTarget(target, min, max); err != nil {
		glog.Errorf("Error updating target: %v", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// GetTarget fetches the object targeted by the given MachineAutoscaler.
func (r *Reconciler) GetTarget(ma *autoscalingv1alpha1.MachineAutoscaler) (*MachineTarget, error) {
	ref := ma.Spec.ScaleTargetRef
	target := &MachineTarget{}

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, err
	}

	gvk := gv.WithKind(ref.Kind)

	if !SupportedTarget(gvk) {
		return nil, ErrUnsupportedTarget
	}

	target.SetGroupVersionKind(gvk)

	// TODO(bison): Support cross namespace references?
	err = r.client.Get(context.TODO(), client.ObjectKey{
		Namespace: ma.Namespace,
		Name:      ref.Name,
	}, &target.Unstructured)

	if err != nil {
		return nil, err
	}

	return target, nil
}

// UpdateTarget updates the min and max annotations on the given target.
func (r *Reconciler) UpdateTarget(target *MachineTarget, min, max int) error {
	// Update the target object's annotations if necessary.
	if target.NeedsUpdate(min, max) {
		target.SetLimits(min, max)

		return r.client.Update(context.TODO(), target)
	}

	return nil
}

// FinalizeTarget handles finalizers for the given target.
func (r *Reconciler) FinalizeTarget(target *MachineTarget) error {
	target.RemoveLimits()
	target.RemoveOwner()

	return r.client.Update(context.TODO(), target)
}

// EnsureFinalizer adds finalizers to the given MachineAutoscaler if necessary.
func (r *Reconciler) EnsureFinalizer(ma *autoscalingv1alpha1.MachineAutoscaler) error {
	for _, f := range ma.GetFinalizers() {
		// Bail early if we already have the finalizer.
		if f == MachineTargetFinalizer {
			return nil
		}
	}

	f := append(ma.GetFinalizers(), MachineTargetFinalizer)
	ma.SetFinalizers(f)

	return r.client.Update(context.TODO(), ma)
}

// RemoveFinalizer removes this packages's finalizers from the given
// MachineAutoscaler instance.
func (r *Reconciler) RemoveFinalizer(ma *autoscalingv1alpha1.MachineAutoscaler) error {
	f := util.FilterString(ma.GetFinalizers(), MachineTargetFinalizer)
	ma.SetFinalizers(f)

	return r.client.Update(context.TODO(), ma)
}

// SupportedTarget indicates whether a GVK is supported as a target.
func SupportedTarget(gvk schema.GroupVersionKind) bool {
	for _, supported := range SupportedTargetGVKs {
		if gvk == supported {
			return true
		}
	}

	return false
}

// targetOwnerRequest is used with handler.EnqueueRequestsFromMapFunc to enqueue
// reconcile requests for the owning MachineAutoscaler of a watched target.
func targetOwnerRequest(a handler.MapObject) []reconcile.Request {
	target, err := MachineTargetFromObject(a.Object)
	if err != nil {
		glog.Errorf("Failed to convert object to MachineTarget: %v", err)
		return nil
	}

	owner, err := target.GetOwner()
	if err != nil {
		glog.V(2).Infof("Will not reconcile: %v", err)
		return nil
	}

	return []reconcile.Request{{NamespacedName: owner}}
}
