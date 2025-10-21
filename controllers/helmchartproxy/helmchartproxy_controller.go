/*
Copyright 2022 The Kubernetes Authors.

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

package helmchartproxy

import (
	"context"
	"slices"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	addonsv1alpha1 "sigs.k8s.io/cluster-api-addon-provider-helm/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// HelmChartProxyReconciler reconciles a HelmChartProxy object.
type HelmChartProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// helmReleaseProxyRolloutMeta is used to gather HelmReleaseProxy  rollout
// metadata for matching clusters.
type helmReleaseProxyRolloutMeta struct {
	cluster clusterv1.Cluster

	// Identifies whether HelmReleaseProxy exists for the cluster.
	hrpExists bool

	// Identifies whether HelmReleaseProxy's ready condition is True.
	hrpReady bool
}

// installOrUpgrade defines the install vs upgrade rolling reconcile type event.
type installOrUpgrade string

const (
	install                installOrUpgrade = "install"
	upgrade                installOrUpgrade = "upgrade"
	hrpRolloutCompletedMsg                  = "HelmChartProxy does not use rollout"
)

// SetupWithManager sets up the controller with the Manager.
func (r *HelmChartProxyReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := ctrl.LoggerFrom(ctx)

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&addonsv1alpha1.HelmChartProxy{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(mgr.GetScheme(), log, r.WatchFilterValue)).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.ClusterToHelmChartProxiesMapper),
		).
		Watches(
			&addonsv1alpha1.HelmReleaseProxy{},
			handler.EnqueueRequestsFromMapFunc(HelmReleaseProxyToHelmChartProxyMapper),
		).
		Complete(r)
}

//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmchartproxies/finalizers,verbs=update
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=helmreleaseproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=list;watch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,verbs=list;get;watch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;bootstrap.cluster.x-k8s.io;controlplane.cluster.x-k8s.io;clusterctl.cluster.x-k8s.io,resources=*,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=list;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *HelmChartProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var reterr error
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Beginning reconciliation for HelmChartProxy", "requestNamespace", req.Namespace, "requestName", req.Name)

	// Fetch the HelmChartProxy instance.
	helmChartProxy := &addonsv1alpha1.HelmChartProxy{}
	if err := r.Get(ctx, req.NamespacedName, helmChartProxy); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(2).Info("HelmChartProxy resource not found, skipping reconciliation", "helmChartProxy", req.NamespacedName)
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	// TODO: should patch helper return an error when the object has been deleted?
	patchHelper, err := patch.NewHelper(helmChartProxy, r.Client)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to init patch helper")
	}

	defer func() {
		log.V(2).Info("Preparing to patch HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
		if err := patchHelmChartProxy(ctx, patchHelper, helmChartProxy); err != nil && reterr == nil {
			reterr = err
			log.Error(err, "failed to patch HelmChartProxy", "helmChartProxy", helmChartProxy.Name)

			return
		}
		log.V(2).Info("Successfully patched HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
	}()

	selector := helmChartProxy.Spec.ClusterSelector

	log.V(2).Info("Finding matching clusters for HelmChartProxy with selector selector", "helmChartProxy", helmChartProxy.Name, "selector", selector)
	// TODO: When a Cluster is being deleted, it will show up in the list of clusters even though we can't Reconcile on it.
	// This is because of ownerRefs and how the Cluster gets deleted. It will be eventually consistent but it would be better
	// to not have errors. An idea would be to check the deletion timestamp.
	clusterList, err := r.listClustersWithLabels(ctx, helmChartProxy.Namespace, selector)
	if err != nil {
		conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.ClusterSelectionFailedReason, clusterv1.ConditionSeverityError, "%s", err.Error())

		return ctrl.Result{}, err
	}
	// conditions.MarkTrue(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsReadyCondition)
	helmChartProxy.SetMatchingClusters(clusterList.Items)

	log.V(2).Info("Finding HelmRelease for HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
	label := map[string]string{
		addonsv1alpha1.HelmChartProxyLabelName: helmChartProxy.Name,
	}
	releaseList, err := r.listInstalledReleases(ctx, helmChartProxy.Namespace, label)
	if err != nil {
		return ctrl.Result{}, err
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if helmChartProxy.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer) {
			controllerutil.AddFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer)
			if err := patchHelmChartProxy(ctx, patchHelper, helmChartProxy); err != nil {
				// TODO: Should we try to set the error here? If we can't add the finalizer we likely can't update the status either.
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			if result, err := r.reconcileDelete(ctx, helmChartProxy, releaseList.Items); err != nil || !result.IsZero() {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(helmChartProxy, addonsv1alpha1.HelmChartProxyFinalizer)
			if err := patchHelmChartProxy(ctx, patchHelper, helmChartProxy); err != nil {
				// TODO: Should we try to set the error here? If we can't remove the finalizer we likely can't update the status either.
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	log.V(2).Info("Reconciling HelmChartProxy", "randomName", helmChartProxy.Name)
	res, err := r.reconcileNormal(ctx, helmChartProxy, clusterList.Items, releaseList.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	conditions.MarkTrue(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition)

	err = r.aggregateHelmReleaseProxyReadyCondition(ctx, helmChartProxy)
	if err != nil {
		log.Error(err, "failed to aggregate HelmReleaseProxy ready condition", "helmChartProxy", helmChartProxy.Name)
		return ctrl.Result{}, err
	}

	return res, nil
}

// reconcileNormal handles the reconciliation of a HelmChartProxy when it is not being deleted. It takes a list of selected Clusters and HelmReleaseProxies
// to uninstall the Helm chart from any Clusters that are no longer selected and to install or update the Helm chart on any Clusters that currently selected.
func (r *HelmChartProxyReconciler) reconcileNormal(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, clusters []clusterv1.Cluster, helmReleaseProxies []addonsv1alpha1.HelmReleaseProxy) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Starting reconcileNormal for chart proxy", "name", helmChartProxy.Name, "strategy", helmChartProxy.Spec.ReconcileStrategy)

	// If Reconcile strategy is not InstallOnce, delete orphaned HelmReleaseProxies
	if helmChartProxy.Spec.ReconcileStrategy != string(addonsv1alpha1.ReconcileStrategyInstallOnce) {
		err := r.deleteOrphanedHelmReleaseProxies(ctx, helmChartProxy, clusters, helmReleaseProxies)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if helmChartProxy.Spec.Rollout == nil {
		// RolloutStepSize is undefined. Set HelmReleaseProxiesRolloutCompletedCondition to True with reason.
		conditions.MarkTrueWithNegativePolarity(
			helmChartProxy,
			addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition,
			addonsv1alpha1.HelmReleaseProxiesRolloutUndefinedReason,
			clusterv1.ConditionSeverityInfo,
			hrpRolloutCompletedMsg,
		)

		for _, cluster := range clusters {
			err := r.reconcileForCluster(ctx, helmChartProxy, cluster)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if helmChartProxy.GetGeneration() == 1 {
		if helmChartProxy.Spec.Rollout.Install == nil {
			// RolloutStepSize is undefined. Set HelmReleaseProxiesRolloutCompletedCondition to True with reason.
			conditions.MarkTrueWithNegativePolarity(
				helmChartProxy,
				addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition,
				addonsv1alpha1.HelmReleaseProxiesRolloutUndefinedReason,
				clusterv1.ConditionSeverityInfo,
				hrpRolloutCompletedMsg,
			)

			for _, cluster := range clusters {
				err := r.reconcileForCluster(ctx, helmChartProxy, cluster)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}

		// rollout with install rollout options.
		return r.rolloutReconcile(ctx, helmChartProxy, clusters, helmReleaseProxies, install)
	}

	if helmChartProxy.Spec.Rollout.Upgrade == nil {
		// RolloutStepSize is undefined. Set HelmReleaseProxiesRolloutCompletedCondition to True with reason.
		conditions.MarkTrueWithNegativePolarity(
			helmChartProxy,
			addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition,
			addonsv1alpha1.HelmReleaseProxiesRolloutUndefinedReason,
			clusterv1.ConditionSeverityInfo,
			hrpRolloutCompletedMsg,
		)

		for _, cluster := range clusters {
			err := r.reconcileForCluster(ctx, helmChartProxy, cluster)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// rollout with upgrade rollout options.
	return r.rolloutReconcile(ctx, helmChartProxy, clusters, helmReleaseProxies, upgrade)
}

// rolloutReconcile is used to rollout changes to matching clusters defined as
// per rollout options corresponding to the kind of change ie; install vs
// upgrade.
func (r *HelmChartProxyReconciler) rolloutReconcile(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, clusters []clusterv1.Cluster, helmReleaseProxies []addonsv1alpha1.HelmReleaseProxy, installOrUpgrade installOrUpgrade) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	var rolloutOptions *addonsv1alpha1.RolloutOptions

	switch installOrUpgrade {
	case install:
		rolloutOptions = helmChartProxy.Spec.Rollout.Install
	case upgrade:
		rolloutOptions = helmChartProxy.Spec.Rollout.Upgrade
	}
	log.V(2).Info("Starting rolloutReconcile for chart proxy", "name", helmChartProxy.Name, "installOrUpgrade", installOrUpgrade)

	// This condition won't be true in normal cases.
	if rolloutOptions == nil {
		return ctrl.Result{}, nil
	}

	var rolloutCount int
	if helmChartProxy.Status.Rollout != nil {
		rolloutCount = ptr.Deref(helmChartProxy.Status.Rollout.Count, rolloutCount)
	}

	if len(clusters) == rolloutCount {
		// RolloutStepSize is defined and all HelmReleaseProxies have been rolled out.
		conditions.MarkTrue(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition)

		return ctrl.Result{}, nil
	}

	// Set HelmReleaseProxiesRolloutCompletedCondition to false as
	// HelmReleaseProxies are being rolled out.
	conditions.MarkFalse(
		helmChartProxy,
		addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition,
		addonsv1alpha1.HelmReleaseProxiesRolloutNotCompleteReason,
		clusterv1.ConditionSeverityInfo,
		"%d Helm release proxies not yet rolled out",
		len(clusters)-rolloutCount,
	)

	// Identifies clusters by their NamespacedName and gathers their
	// helmReleaseProxyRolloutMeta.
	clusterNnRolloutMeta := map[string]*helmReleaseProxyRolloutMeta{}
	for _, c := range clusters {
		nn := getNamespacedNameStringFor(c.Namespace, c.Name)
		clusterNnRolloutMeta[nn] = &helmReleaseProxyRolloutMeta{
			cluster: c,
		}
	}
	for _, h := range helmReleaseProxies {
		ref := h.Spec.ClusterRef
		nn := getNamespacedNameStringFor(ref.Namespace, ref.Name)
		meta := clusterNnRolloutMeta[nn]
		meta.hrpExists = true
		meta.hrpReady = conditions.IsTrue(&h, addonsv1alpha1.HelmReleaseReadyCondition)
	}

	// Sort helmReleaseProxy rollout metadata by cluster namespaced name to
	// ensure orderliness.
	rolloutMetaSorted := make([]*helmReleaseProxyRolloutMeta, len(clusterNnRolloutMeta))
	i := 0
	for _, m := range clusterNnRolloutMeta {
		rolloutMetaSorted[i] = m
		i++
	}
	for m := range clusterNnRolloutMeta {
		delete(clusterNnRolloutMeta, m)
	}

	slices.SortStableFunc(rolloutMetaSorted, func(a, b *helmReleaseProxyRolloutMeta) int {
		nnA := getNamespacedNameStringFor(a.cluster.Namespace, a.cluster.Name)
		nnB := getNamespacedNameStringFor(b.cluster.Namespace, b.cluster.Name)
		if nnA < nnB {
			return -1
		}

		if nnA > nnB {
			return 1
		}

		return 0
	})

	// If HelmReleaseProxiesReadyCondition is Unknown, create the first batch
	// of HelmReleaseProxies and exit.
	if conditions.IsUnknown(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition) {
		if len(helmReleaseProxies) != 0 {
			return ctrl.Result{Requeue: true}, nil
		}

		count := 0
		stepSize, err := intstr.GetScaledValueFromIntOrPercent(rolloutOptions.StepInit, len(clusters), true)
		if err != nil {
			return ctrl.Result{}, err
		}

		defer func() {
			log.V(2).Info("Updating rollout status", "name", helmChartProxy.Name, "HelmReleaseProxiesReadyCondition", corev1.ConditionUnknown, "count", count, "stepSize", stepSize)
			helmChartProxy.Status.Rollout = &addonsv1alpha1.RolloutStatus{Count: ptr.To(count), StepSize: ptr.To(stepSize)}
		}()

		// If HelmReleaseProxiesReadyCondition is Unknown and the first batch of HelmReleaseProxies have
		// been created, then exit early.
		if stepSize == len(helmReleaseProxies) {
			return ctrl.Result{Requeue: true}, nil
		}

		for _, meta := range rolloutMetaSorted {
			// The first batch of helmReleaseProxies have been reconciled.
			if count >= stepSize {
				return ctrl.Result{Requeue: true}, nil
			}

			err := r.reconcileForCluster(ctx, helmChartProxy, meta.cluster)
			log.V(2).Info("Reconciling for cluster", "name", helmChartProxy.Name, "HelmReleaseProxiesReadyCondition", corev1.ConditionUnknown, "cluster", meta.cluster.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			count++
		}

		// In cases where the count of remaining HelmReleaseProxies to be rolled
		// out is less than rollout step size.
		return ctrl.Result{Requeue: true}, nil
	}

	// If HelmReleaseProxiesReadyCondition is false, reconcile existing
	// HelmReleaseProxies and exit.
	if conditions.IsFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition) {
		log.V(2).Info("HelmReleaseProxiesReady condition false; reconciling existing HelmReleaseProxies", "name", helmChartProxy.Name)

		for _, meta := range rolloutMetaSorted {
			if meta.hrpExists {
				err := r.reconcileForCluster(ctx, helmChartProxy, meta.cluster)
				log.V(2).Info("Reconciling for cluster", "name", helmChartProxy.Name, "HelmReleaseProxiesReadyCondition", corev1.ConditionFalse, "cluster", meta.cluster.Name)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		return ctrl.Result{Requeue: true}, nil
	}

	log.V(2).Info("HelmReleaseProxiesReady condition true; proceeding to reconcile the next batch of HelmReleaseProxies", "name", helmChartProxy.Name)
	// HelmReleaseProxyReadyCondition is True; continue with reconciling the
	// next batch of HelmReleaseProxies.
	var oldStepSize int
	if helmChartProxy.Status.Rollout != nil {
		oldStepSize = ptr.Deref(helmChartProxy.Status.Rollout.StepSize, oldStepSize)
	}

	var stepIncrement int
	var err error
	if rolloutOptions.StepIncrement != nil {
		stepIncrement, err = intstr.GetScaledValueFromIntOrPercent(rolloutOptions.StepIncrement, len(clusters), true)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	var stepLimit int
	if rolloutOptions.StepLimit != nil {
		stepLimit, err = intstr.GetScaledValueFromIntOrPercent(rolloutOptions.StepLimit, len(clusters), true)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	var stepInit int
	if rolloutOptions.StepInit != nil {
		stepInit, err = intstr.GetScaledValueFromIntOrPercent(rolloutOptions.StepInit, len(clusters), true)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	stepSize := oldStepSize + stepIncrement
	if stepLimit > stepInit && stepSize > stepLimit {
		stepSize = stepLimit
	}

	count := 0
	defer func() {
		var oldCount int
		if helmChartProxy.Status.Rollout != nil {
			oldCount = ptr.Deref(helmChartProxy.Status.Rollout.Count, oldCount)
		}
		newCount := oldCount + count
		log.V(2).Info("Updating rollout status", "name", helmChartProxy.Name, "HelmReleaseProxiesReadyCondition", corev1.ConditionTrue, "count", newCount, "stepSize", stepSize)
		helmChartProxy.Status.Rollout = &addonsv1alpha1.RolloutStatus{Count: ptr.To(newCount), StepSize: ptr.To(stepSize)}
	}()

	for _, meta := range rolloutMetaSorted {
		// Exit if HelmReleaseProxyReadyCondition has not caught up to existing
		// HelmReleaseProxies status.
		if meta.hrpExists && !meta.hrpReady {
			return ctrl.Result{Requeue: true}, nil
		}

		// The next batch of helmReleaseProxies have been reconciled.
		if count >= stepSize {
			return ctrl.Result{Requeue: true}, nil
		}

		// Skip reconciling the cluster if its HelmReleaseProxy already exists.
		if meta.hrpExists {
			continue
		}
		err := r.reconcileForCluster(ctx, helmChartProxy, meta.cluster)
		log.V(2).Info("Reconciling for cluster", "name", helmChartProxy.Name, "HelmReleaseProxiesReadyCondition", corev1.ConditionTrue, "cluster", meta.cluster.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		count++
	}

	// In cases where the count of remaining HelmReleaseProxies to be rolled
	// out is less than rollout step size.
	return ctrl.Result{Requeue: true}, nil
}

// reconcileDelete handles the deletion of a HelmChartProxy. It takes a list of HelmReleaseProxies to uninstall the Helm chart from all selected Clusters.
func (r *HelmChartProxyReconciler) reconcileDelete(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy, releases []addonsv1alpha1.HelmReleaseProxy) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	getters := make([]conditions.Getter, 0)

	log.V(2).Info("Deleting all HelmReleaseProxies as part of HelmChartProxy deletion", "helmChartProxy", helmChartProxy.Name)
	for i := range releases {
		release := releases[i]

		log.V(2).Info("Deleting release", "releaseName", release.Name, "cluster", release.Spec.ClusterRef.Name)
		if err := r.deleteHelmReleaseProxy(ctx, &release); err != nil {
			// TODO: will this fail if clusterRef is nil
			return ctrl.Result{}, errors.Wrapf(err, "failed to delete release %s from cluster %s", release.Name, release.Spec.ClusterRef.Name)
		}

		log.V(2).Info("Validating release deletion", "releaseName", release.Name)
		if err := r.Get(ctx, client.ObjectKeyFromObject(&release), &release); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return ctrl.Result{}, errors.Wrapf(err, "failed to get HelmReleaseProxy %s", release.Name)
		}

		log.V(2).Info("The release has not been deleted yet, waiting for it to be removed", "releaseName", release.Name)
		getters = append(getters, &release)
	}

	if len(getters) > 0 {
		conditions.SetAggregate(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition, getters, conditions.AddSourceRef(), conditions.WithStepCounterIf(false))

		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// listClustersWithLabels returns a list of Clusters that match the given label selector.
func (r *HelmChartProxyReconciler) listClustersWithLabels(ctx context.Context, namespace string, selector metav1.LabelSelector) (*clusterv1.ClusterList, error) {
	clusterList := &clusterv1.ClusterList{}
	// To support for the matchExpressions field, convert LabelSelector to labels.Selector to specify labels.Selector for ListOption. (Issue #15)
	labelselector, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, err
	}

	if err := r.List(ctx, clusterList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: labelselector}); err != nil {
		return nil, err
	}

	return clusterList, nil
}

// listInstalledReleases returns a list of HelmReleaseProxies that match the given label selector.
func (r *HelmChartProxyReconciler) listInstalledReleases(ctx context.Context, namespace string, labels map[string]string) (*addonsv1alpha1.HelmReleaseProxyList, error) {
	releaseList := &addonsv1alpha1.HelmReleaseProxyList{}

	// TODO: should we use client.MatchingLabels or try to use the labelSelector itself?
	if err := r.List(ctx, releaseList, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	return releaseList, nil
}

// aggregateHelmReleaseProxyReadyCondition HelmReleaseProxyReadyCondition from all HelmReleaseProxies that match the given label selector.
func (r *HelmChartProxyReconciler) aggregateHelmReleaseProxyReadyCondition(ctx context.Context, helmChartProxy *addonsv1alpha1.HelmChartProxy) error {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Aggregating HelmReleaseProxyReadyCondition")

	labels := map[string]string{
		addonsv1alpha1.HelmChartProxyLabelName: helmChartProxy.Name,
	}
	releaseList, err := r.listInstalledReleases(ctx, helmChartProxy.Namespace, labels)
	if err != nil {
		// conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition, addonsv1alpha1.HelmReleaseProxyListFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return err
	}

	if len(releaseList.Items) == 0 {
		// Consider it to be vacuously true if there are no releases. This should only be reached if we previously had HelmReleaseProxies but they were all deleted
		// due to the Clusters being unselected. In that case, we should consider the condition to be true.
		conditions.MarkTrue(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition)
		return nil
	}

	getters := make([]conditions.Getter, 0, len(releaseList.Items))
	for i := range releaseList.Items {
		helmReleaseProxy := &releaseList.Items[i]
		if helmReleaseProxy.Generation != helmReleaseProxy.Status.ObservedGeneration {
			conditions.MarkFalse(helmChartProxy, addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition, addonsv1alpha1.HelmReleaseProxySpecsUpdatingReason, clusterv1.ConditionSeverityInfo, "Helm release proxy '%s' is not updated yet", helmReleaseProxy.Name)
			return nil
		}
		getters = append(getters, helmReleaseProxy)
	}

	conditions.SetAggregate(helmChartProxy, addonsv1alpha1.HelmReleaseProxiesReadyCondition, getters, conditions.AddSourceRef(), conditions.WithStepCounterIf(false))

	return nil
}

// patchHelmChartProxy patches the HelmChartProxy object and sets the ReadyCondition as an aggregate of the other condition set.
// TODO: Is this preferable to client.Update() calls? Based on testing it seems like it avoids race conditions.
func patchHelmChartProxy(ctx context.Context, patchHelper *patch.Helper, helmChartProxy *addonsv1alpha1.HelmChartProxy) error {
	conditions.SetSummary(helmChartProxy,
		conditions.WithConditions(
			addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition,
			addonsv1alpha1.HelmReleaseProxiesReadyCondition,
			addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition,
		),
	)

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		helmChartProxy,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			addonsv1alpha1.HelmReleaseProxySpecsUpToDateCondition,
			addonsv1alpha1.HelmReleaseProxiesReadyCondition,
			addonsv1alpha1.HelmReleaseProxiesRolloutCompletedCondition,
		}},
		patch.WithStatusObservedGeneration{},
	)
}

// ClusterToHelmChartProxiesMapper is a mapper function that maps a Cluster to the HelmChartProxies that would select the Cluster.
func (r *HelmChartProxyReconciler) ClusterToHelmChartProxiesMapper(ctx context.Context, o client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	cluster, ok := o.(*clusterv1.Cluster)
	if !ok {
		// Suppress the error for now
		log.Error(errors.Errorf("expected a Cluster but got %T", o), "failed to map object to HelmChartProxy")
		return nil
	}

	helmChartProxies := &addonsv1alpha1.HelmChartProxyList{}

	// TODO: Figure out if we want this search to be cross-namespaces.

	if err := r.List(ctx, helmChartProxies, client.InNamespace(cluster.Namespace)); err != nil {
		return nil
	}

	results := []ctrl.Request{}
	for _, helmChartProxy := range helmChartProxies.Items {
		selector, err := metav1.LabelSelectorAsSelector(&helmChartProxy.Spec.ClusterSelector)
		if err != nil {
			// Suppress the error for now
			log.Error(err, "failed to parse ClusterSelector for HelmChartProxy", "helmChartProxy", helmChartProxy.Name)
			return nil
		}

		if selector.Matches(labels.Set(cluster.Labels)) {
			results = append(results, ctrl.Request{
				// The HelmReleaseProxy is always in the same namespace as the HelmChartProxy.
				NamespacedName: client.ObjectKey{Namespace: helmChartProxy.Namespace, Name: helmChartProxy.Name},
			})
		}
	}

	return results
}

// HelmReleaseProxyToHelmChartProxyMapper is a mapper function that maps a HelmReleaseProxy to the HelmChartProxy that owns it.
// This is used to trigger an update of the HelmChartProxy when a HelmReleaseProxy is changed.
func HelmReleaseProxyToHelmChartProxyMapper(ctx context.Context, o client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	helmReleaseProxy, ok := o.(*addonsv1alpha1.HelmReleaseProxy)
	if !ok {
		// Suppress the error for now
		log.Error(errors.Errorf("expected a HelmReleaseProxy but got %T", o), "failed to map object to HelmChartProxy")
		return nil
	}

	// Check if the controller reference is already set and
	// return an empty result when one is found.
	for _, ref := range helmReleaseProxy.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			name := client.ObjectKey{
				Namespace: helmReleaseProxy.GetNamespace(),
				Name:      ref.Name,
			}

			return []ctrl.Request{
				{
					NamespacedName: name,
				},
			}
		}
	}

	return nil
}

// getNamespacedNameStringFor to retrieve the namespaced name as a string.
func getNamespacedNameStringFor(namespace, name string) string {
	return types.NamespacedName{Namespace: namespace, Name: name}.String()
}
