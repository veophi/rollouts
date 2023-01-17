/*
Copyright 2022 The Kruise Authors.

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

package deployment

import (
	"context"
	"encoding/json"

	"github.com/openkruise/rollouts/api/v1alpha1"
	batchcontext "github.com/openkruise/rollouts/pkg/controller/batchrelease/context"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/control/partitionstyle"
	deploymentutil "github.com/openkruise/rollouts/pkg/controller/deployment/util"
	"github.com/openkruise/rollouts/pkg/util"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type realController struct {
	*util.WorkloadInfo
	client client.Client
	pods   []*corev1.Pod
	key    types.NamespacedName
	object *apps.Deployment
}

func NewController(cli client.Client, key types.NamespacedName, _ schema.GroupVersionKind) partitionstyle.Interface {
	return &realController{
		key:    key,
		client: cli,
	}
}

func (rc *realController) GetInfo() *util.WorkloadInfo {
	return rc.WorkloadInfo
}

func (rc *realController) BuildController() (partitionstyle.Interface, error) {
	if rc.object != nil {
		return rc, nil
	}
	object := &apps.Deployment{}
	if err := rc.client.Get(context.TODO(), rc.key, object); err != nil {
		return rc, err
	}
	rc.object = object
	workloadInfo, err := rc.getWorkloadInfo(object)
	if err != nil {
		return nil, err
	}
	rc.WorkloadInfo = workloadInfo
	return rc, nil
}

func (rc *realController) ListOwnedPods() ([]*corev1.Pod, error) {
	if rc.pods != nil {
		return rc.pods, nil
	}
	var err error
	rc.pods, err = util.ListOwnedPods(rc.client, rc.object)
	return rc.pods, err
}

func (rc *realController) Initialize(release *v1alpha1.BatchRelease) error {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d := &apps.Deployment{}
		if err := rc.client.Get(context.TODO(), rc.key, d); err != nil {
			return err
		}
		if control.IsControlledByBatchRelease(release, d) {
			return nil
		}
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		// set strategy to annotations
		strategy := util.UnmarshalledDeploymentStrategy(d)
		rollingUpdate := strategy.RollingUpdate
		if d.Spec.Strategy.RollingUpdate != nil {
			rollingUpdate = d.Spec.Strategy.RollingUpdate
		}
		d.Annotations[v1alpha1.DeploymentStrategyAnnotation] = util.MarshalledDeploymentStrategy(
			v1alpha1.PartitionRollingStyle, rollingUpdate, intstr.FromInt(0), false)
		// claim the deployment is under our control
		owner, _ := json.Marshal(metav1.NewControllerRef(release, release.GetObjectKind().GroupVersionKind()))
		d.Annotations[util.BatchReleaseControlAnnotation] = string(owner)
		// disable the native deployment controller
		d.Spec.Paused = true
		d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
		return rc.client.Update(context.TODO(), d)
	}); err != nil {
		return err
	}

	klog.Infof("Successfully initialized Deployment %v", rc.key)
	return nil
}

func (rc *realController) UpgradeBatch(ctx *batchcontext.BatchContext) error {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d := &apps.Deployment{}
		if err := rc.client.Get(context.TODO(), rc.key, d); err != nil {
			return err
		}
		if !deploymentutil.IsUnderRolloutControl(d) {
			klog.Warningf("Cannot upgrade batch because Deployment %v is not under our control", rc.key)
			return nil
		}
		strategy := util.UnmarshalledDeploymentStrategy(d)
		if control.IsCurrentMoreThanOrEqualToDesired(strategy.Partition, ctx.DesiredPartition) {
			return nil
		}
		strategy.Partition = ctx.DesiredPartition
		strategyAnno, _ := json.Marshal(&strategy)
		d.Annotations[v1alpha1.DeploymentStrategyAnnotation] = string(strategyAnno)
		return rc.client.Update(context.TODO(), d)
	}); err != nil {
		return err
	}

	klog.Infof("Successfully submit partition %v for Deployment %v", ctx.DesiredPartition, rc.key)
	return nil
}

func (rc *realController) Finalize(release *v1alpha1.BatchRelease) error {
	if rc.object == nil {
		return nil
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d := &apps.Deployment{}
		if err := rc.client.Get(context.TODO(), rc.key, d); err != nil {
			return client.IgnoreNotFound(err)
		}
		if release.Spec.ReleasePlan.BatchPartition == nil {
			d.Spec.Paused = false
			strategy := util.UnmarshalledDeploymentStrategy(d)
			d.Spec.Strategy.Type = apps.RollingUpdateDeploymentStrategyType
			d.Spec.Strategy.RollingUpdate = strategy.RollingUpdate
			delete(d.Annotations, v1alpha1.DeploymentStrategyAnnotation)
			delete(d.Annotations, v1alpha1.DeploymentExtraStatusAnnotation)
			delete(d.Labels, v1alpha1.DeploymentStableRevisionLabel)
		}
		delete(d.Annotations, util.BatchReleaseControlAnnotation)
		return rc.client.Update(context.TODO(), d)
	}); err != nil {
		return err
	}

	klog.Infof("Successfully finalize Deployment %v", klog.KObj(rc.object))
	return nil
}

func (rc *realController) CalculateBatchContext(release *v1alpha1.BatchRelease) (*batchcontext.BatchContext, error) {
	rolloutID := release.Spec.ReleasePlan.RolloutID
	if rolloutID != "" {
		// if rollout-id is set, the pod will be patched batch label,
		// so we have to list pod here.
		if _, err := rc.ListOwnedPods(); err != nil {
			return nil, err
		}
	}

	currentBatch := release.Status.CanaryStatus.CurrentBatch
	desiredPartition := release.Spec.ReleasePlan.Batches[currentBatch].CanaryReplicas
	PlannedUpdatedReplicas := deploymentutil.NewRSReplicasLimit(desiredPartition, rc.object)

	return &batchcontext.BatchContext{
		Pods:             rc.pods,
		RolloutID:        rolloutID,
		CurrentBatch:     currentBatch,
		UpdateRevision:   release.Status.UpdateRevision,
		DesiredPartition: desiredPartition,
		FailureThreshold: release.Spec.ReleasePlan.FailureThreshold,

		Replicas:               rc.Replicas,
		UpdatedReplicas:        rc.Status.UpdatedReplicas,
		UpdatedReadyReplicas:   rc.Status.UpdatedReadyReplicas,
		PlannedUpdatedReplicas: PlannedUpdatedReplicas,
		DesiredUpdatedReplicas: PlannedUpdatedReplicas,
	}, nil
}

func (rc *realController) getWorkloadInfo(d *apps.Deployment) (*util.WorkloadInfo, error) {
	workloadInfo := util.ParseWorkload(d)
	extraStatus := util.UnmarshalledDeploymentExtraStatus(d)
	workloadInfo.Status.UpdatedReadyReplicas = extraStatus.UpdatedReadyReplicas
	finder := util.NewControllerFinder(rc.client)
	rss, err := finder.GetReplicaSetsForDeployment(d)
	if err != nil {
		return nil, err
	}
	_, oldRS := util.FindCanaryAndStableReplicaSet(rss, d)
	if oldRS != nil {
		workloadInfo.Status.StableRevision = oldRS.Labels[apps.DefaultDeploymentUniqueLabelKey]
	}
	return workloadInfo, nil
}
