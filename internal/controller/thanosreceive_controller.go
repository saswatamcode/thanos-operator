/*
Copyright 2024.

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
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	monitoringthanosiov1alpha1 "github.com/thanos-community/thanos-operator/api/v1alpha1"
	"github.com/thanos-community/thanos-operator/internal/pkg/manifests"
	"github.com/thanos-community/thanos-operator/internal/pkg/manifests/receive"

	"github.com/go-logr/logr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	receiveFinalizer = "monitoring.thanos.io/receive-finalizer"
)

// ThanosReceiveReconciler reconciles a ThanosReceive object
type ThanosReceiveReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	logger logr.Logger

	reg                                 prometheus.Registerer
	reconciliationsTotal                prometheus.Counter
	reconciliationsFailedTotal          prometheus.Counter
	hashringsConfigured                 *prometheus.GaugeVec
	endpointWatchesReconciliationsTotal prometheus.Counter
	clientErrorsTotal                   prometheus.Counter
}

// NewThanosReceiveReconciler returns a reconciler for ThanosReceive resources.
func NewThanosReceiveReconciler(logger logr.Logger, client client.Client, scheme *runtime.Scheme, recorder record.EventRecorder, reg prometheus.Registerer) *ThanosReceiveReconciler {
	return &ThanosReceiveReconciler{
		Client:   client,
		Scheme:   scheme,
		Recorder: recorder,

		logger: logger,

		reg: reg,
		reconciliationsTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "thanos_operator_receive_reconciliations_total",
			Help: "Total number of reconciliations for ThanosReceive resources",
		}),
		reconciliationsFailedTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "thanos_operator_receive_reconciliations_failed_total",
			Help: "Total number of failed reconciliations for ThanosReceive resources",
		}),
		hashringsConfigured: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "thanos_operator_receive_hashrings_configured",
			Help: "Total number of configured hashrings for ThanosReceive resources",
		}, []string{"resource", "namespace"}),
		endpointWatchesReconciliationsTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "thanos_operator_receive_endpoint_event_reconciliations_total",
			Help: "Total number of reconciliations for ThanosReceive resources due to EndpointSlice events",
		}),
		clientErrorsTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "thanos_operator_receive_client_errors_total",
			Help: "Total number of errors encountered during kube client calls of ThanosReceive resources",
		}),
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *ThanosReceiveReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.reconciliationsTotal.Inc()

	// Fetch the ThanosReceive instance to validate it is applied on the cluster.
	receiver := &monitoringthanosiov1alpha1.ThanosReceive{}
	err := r.Get(ctx, req.NamespacedName, receiver)
	if err != nil {
		r.clientErrorsTotal.Inc()
		if apierrors.IsNotFound(err) {
			r.logger.Info("thanos receive resource not found. ignoring since object may be deleted")
			return ctrl.Result{}, nil
		}
		r.logger.Error(err, "failed to get ThanosReceive")
		r.reconciliationsFailedTotal.Inc()
		return ctrl.Result{}, err
	}

	// handle object being deleted - inferred from the existence of DeletionTimestamp
	if !receiver.GetDeletionTimestamp().IsZero() {
		return r.handleDeletionTimestamp(receiver)
	}

	err = r.syncResources(ctx, *receiver)
	if err != nil {
		r.reconciliationsFailedTotal.Inc()
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=monitoring.thanos.io,resources=thanosreceives,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.thanos.io,resources=thanosreceives/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.thanos.io,resources=thanosreceives/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="discovery.k8s.io",resources=endpointslices,verbs=get;list;watch

// SetupWithManager sets up the controller with the Manager.
func (r *ThanosReceiveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bld := ctrl.NewControllerManagedBy(mgr)
	return r.buildController(*bld)
}

// buildController sets up the controller with the Manager.
func (r *ThanosReceiveReconciler) buildController(bld builder.Builder) error {
	// add a selector to watch for the endpointslices that are owned by the ThanosReceive ingest Service(s).
	endpointSliceLS := metav1.LabelSelector{
		MatchLabels: map[string]string{manifests.ComponentLabel: receive.IngestComponentName},
	}
	endpointSlicePredicate, err := predicate.LabelSelectorPredicate(endpointSliceLS)
	if err != nil {
		return err
	}

	bld.
		For(&monitoringthanosiov1alpha1.ThanosReceive{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Watches(
			&discoveryv1.EndpointSlice{},
			r.enqueueForEndpointSlice(r.Client),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}, endpointSlicePredicate),
		)

	return bld.Complete(r)
}

// syncResources syncs the resources for the ThanosReceive resource.
// It creates or updates the resources for the hashrings and the router.
func (r *ThanosReceiveReconciler) syncResources(ctx context.Context, receiver monitoringthanosiov1alpha1.ThanosReceive) error {
	var objs []client.Object
	objs = append(objs, r.buildHashrings(receiver)...)

	hashringConf, err := r.buildHashringConfig(ctx, receiver)
	if err != nil {
		if !errors.Is(err, receive.ErrHashringsEmpty) {
			return fmt.Errorf("failed to build hashring configuration: %w", err)
		}
		// we can create the config map even if there are no hashrings
		objs = append(objs, hashringConf)
	} else {
		objs = append(objs, hashringConf)
		// todo bring up the router components only if there are ready hashrings to avoid crash looping the router
	}

	var errCount int32
	for _, obj := range objs {
		if manifests.IsNamespacedResource(obj) {
			obj.SetNamespace(receiver.Namespace)
			if err := ctrl.SetControllerReference(&receiver, obj, r.Scheme); err != nil {
				r.logger.Error(err, "failed to set controller owner reference to resource")
				errCount++
				continue
			}
		}

		desired := obj.DeepCopyObject().(client.Object)
		mutateFn := manifests.MutateFuncFor(obj, desired)

		op, err := ctrl.CreateOrUpdate(ctx, r.Client, obj, mutateFn)
		if err != nil {
			r.logger.Error(
				err, "failed to create or update resource",
				"gvk", obj.GetObjectKind().GroupVersionKind().String(),
				"resource", obj.GetName(),
				"namespace", obj.GetNamespace(),
			)
			errCount++
			continue
		}

		r.logger.V(1).Info(
			"resource configured",
			"operation", op, "gvk", obj.GetObjectKind().GroupVersionKind().String(),
			"resource", obj.GetName(), "namespace", obj.GetNamespace(),
		)
	}

	if errCount > 0 {
		r.clientErrorsTotal.Add(float64(errCount))
		return fmt.Errorf("failed to create or update %d resources for the hashrings", errCount)
	}

	return nil
}

// build hashring builds out the ingesters for the ThanosReceive resource.
func (r *ThanosReceiveReconciler) buildHashrings(receiver monitoringthanosiov1alpha1.ThanosReceive) []client.Object {
	opts := make([]receive.IngesterOptions, 0)
	baseLabels := receiver.GetLabels()
	baseSecret := receiver.Spec.Ingester.DefaultObjectStorageConfig.ToSecretKeySelector()

	for _, hashring := range receiver.Spec.Ingester.Hashrings {
		objStoreSecret := baseSecret
		if hashring.ObjectStorageConfig != nil {
			objStoreSecret = hashring.ObjectStorageConfig.ToSecretKeySelector()
		}

		metaOpts := manifests.Options{
			Name:      receive.IngesterNameFromParent(receiver.GetName(), hashring.Name),
			Namespace: receiver.GetNamespace(),
			Replicas:  hashring.Replicas,
			Labels:    manifests.MergeLabels(baseLabels, hashring.Labels),
			Image:     receiver.Spec.Image,
			LogLevel:  receiver.Spec.LogLevel,
			LogFormat: receiver.Spec.LogFormat,
		}.ApplyDefaults()

		opt := receive.IngesterOptions{
			Options:        metaOpts,
			Retention:      string(*hashring.Retention),
			StorageSize:    resource.MustParse(hashring.StorageSize),
			ObjStoreSecret: objStoreSecret,
			ExternalLabels: hashring.ExternalLabels,
		}
		opts = append(opts, opt)
	}

	return receive.BuildIngesters(opts)
}

// buildHashringConfig builds the hashring configuration for the ThanosReceive resource.
func (r *ThanosReceiveReconciler) buildHashringConfig(ctx context.Context, receiver monitoringthanosiov1alpha1.ThanosReceive) (client.Object, error) {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: receiver.GetNamespace(), Name: receiver.GetName()}, cm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get config map for resource %s: %w", receiver.GetName(), err)
		}
	}

	opts := receive.HashringOptions{
		Options: manifests.Options{
			Name:      receiver.GetName(),
			Namespace: receiver.GetNamespace(),
			Labels:    receiver.GetLabels(),
		},
		DesiredReplicationFactor: receiver.Spec.Router.ReplicationFactor,
		HashringSettings:         make(map[string]receive.HashringMeta, len(receiver.Spec.Ingester.Hashrings)),
	}

	totalHashrings := len(receiver.Spec.Ingester.Hashrings)
	for i, hashring := range receiver.Spec.Ingester.Hashrings {
		labelValue := receive.IngesterNameFromParent(receiver.GetName(), hashring.Name)
		// kubernetes sets this label on the endpoint slices - we want to match the generated name
		selectorListOpt := client.MatchingLabels{discoveryv1.LabelServiceName: labelValue}

		eps := discoveryv1.EndpointSliceList{}
		if err = r.Client.List(ctx, &eps, selectorListOpt, client.InNamespace(receiver.GetNamespace())); err != nil {
			return nil, fmt.Errorf("failed to list endpoint slices for resource %s: %w", receiver.GetName(), err)
		}

		opts.HashringSettings[labelValue] = receive.HashringMeta{
			DesiredReplicasReplicas:  hashring.Replicas,
			OriginalName:             hashring.Name,
			Tenants:                  hashring.Tenants,
			TenantMatcherType:        receive.TenantMatcher(hashring.TenantMatcherType),
			AssociatedEndpointSlices: eps,
			// set the priority by slice order for now
			Priority: totalHashrings - i,
		}
	}

	r.hashringsConfigured.WithLabelValues(receiver.GetName(), receiver.GetNamespace()).Set(float64(totalHashrings))
	return receive.BuildHashrings(r.logger, cm, opts)
}

func (r *ThanosReceiveReconciler) handleDeletionTimestamp(receiveHashring *monitoringthanosiov1alpha1.ThanosReceive) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(receiveHashring, receiveFinalizer) {
		r.logger.Info("performing Finalizer Operations for ThanosReceiveHashring before delete CR")

		r.Recorder.Event(receiveHashring, "Warning", "Deleting",
			fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
				receiveHashring.Name,
				receiveHashring.Namespace))
	}
	return ctrl.Result{}, nil
}

// enqueueForEndpointSlice enqueues requests for the ThanosReceive resource when an EndpointSlice event is triggered.
func (r *ThanosReceiveReconciler) enqueueForEndpointSlice(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {

		if len(obj.GetOwnerReferences()) != 1 || obj.GetOwnerReferences()[0].Kind != "Service" {
			return nil
		}

		svc := &corev1.Service{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetOwnerReferences()[0].Name}, svc); err != nil {
			return nil
		}

		if len(svc.GetOwnerReferences()) != 1 || svc.GetOwnerReferences()[0].Kind != "ThanosReceive" {
			return nil
		}

		r.endpointWatchesReconciliationsTotal.Inc()
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      svc.GetOwnerReferences()[0].Name,
				},
			},
		}
	})
}
