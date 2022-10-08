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

package workloads

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/workloads/deploymentinplacecontroller"
	deploymentutil "github.com/openkruise/rollouts/pkg/controller/batchrelease/workloads/deploymentinplacecontroller/util"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/openkruise/rollouts/api/v1alpha1"
	"github.com/openkruise/rollouts/pkg/util"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeploymentInPlaceController is responsible for handling rollout deployment type of workloads
type DeploymentInPlaceController struct {
	deploymentinplacecontroller.DeploymentController
	deploymentKey types.NamespacedName
	deployment    *appsv1.Deployment
	newStatus     *v1alpha1.BatchReleaseStatus
}

// NewDeploymentInPlaceController creates a new deployment rollout controller
func NewDeploymentInPlaceController(cli client.Client, recorder record.EventRecorder, release *v1alpha1.BatchRelease, newStatus *v1alpha1.BatchReleaseStatus, targetNamespacedName types.NamespacedName) *DeploymentInPlaceController {
	return &DeploymentInPlaceController{
		DeploymentController: *deploymentinplacecontroller.NewDeploymentController(cli, recorder, release),
		deploymentKey:        targetNamespacedName,
		newStatus:            newStatus,
	}
}

// VerifyWorkload verifies that the workload is ready to execute release plan
func (c *DeploymentInPlaceController) VerifyWorkload() (bool, error) {
	var err error
	var message string
	defer func() {
		if err != nil {
			c.EventRecorder.Event(c.Release, v1.EventTypeWarning, "VerifyFailed", err.Error())
		} else if message != "" {
			klog.Warningf(message)
		}
	}()

	if err = c.fetchDeployment(); err != nil {
		message = err.Error()
		return false, err
	}

	// if the workload status is untrustworthy
	if c.deployment.Status.ObservedGeneration != c.deployment.Generation {
		message = fmt.Sprintf("deployment(%v) is still reconciling, wait for it to be done", c.deploymentKey)
		return false, nil
	}

	// if the deployment has been promoted, no need to go on
	if deploymentutil.DeploymentComplete(c.deployment, &c.deployment.Status) {
		message = fmt.Sprintf("deployment(%v) update revision has been promoted, no need to reconcile", c.deploymentKey)
		return false, nil
	}

	// if the deployment is not paused and is not under our control
	if !c.deployment.Spec.Paused {
		message = fmt.Sprintf("Deployment(%v) should be paused before execute the release plan", c.deploymentKey)
		return false, nil
	}

	c.EventRecorder.Event(c.Release, v1.EventTypeNormal, "VerifiedSuccessfully", "ReleasePlan and the deployment resource are verified")
	return true, nil
}

// PrepareBeforeProgress makes sure that the source and target deployment is under our control
func (c *DeploymentInPlaceController) PrepareBeforeProgress() (bool, error) {
	if err := c.fetchDeployment(); err != nil {
		return false, err
	}

	// claim the deployment is under our control
	if _, err := c.claimDeployment(c.deployment); err != nil {
		return false, err
	}

	// record revisions and replicas info to BatchRelease.Status
	c.recordDeploymentRevisionAndReplicas()

	c.EventRecorder.Event(c.Release, v1.EventTypeNormal, "InitializedSuccessfully", "Rollout resource are initialized")
	return true, nil
}

// UpgradeOneBatch calculates the number of pods we can upgrade once according to the rollout spec
// and then set the partition accordingly
func (c *DeploymentInPlaceController) UpgradeOneBatch() (bool, error) {
	if err := c.fetchDeployment(); err != nil {
		return false, err
	}

	done, err := c.SyncDeployment(c.deployment)
	if !done || err != nil {
		return done, err
	}

	if done {
		c.EventRecorder.Eventf(c.Release, v1.EventTypeNormal, "SetBatchDone", "Finished submitting all upgrade quests for batch %d", c.newStatus.CanaryStatus.CurrentBatch)
	}
	return done, nil
}

// CheckOneBatchReady checks to see if the pods are all available according to the rollout plan
func (c *DeploymentInPlaceController) CheckOneBatchReady() (bool, error) {
	if err := c.fetchDeployment(); err != nil {
		return false, err
	}

	ready, err := c.IsBatchReady(c.deployment)
	if err != nil {
		return ready, err
	}

	if ready {
		c.EventRecorder.Eventf(c.Release, v1.EventTypeNormal, "BatchReady", "Batch %d is ready", c.newStatus.CanaryStatus.CurrentBatch)
	}
	return ready, nil
}

// FinalizeProgress makes sure the deployment is all upgraded
func (c *DeploymentInPlaceController) FinalizeProgress(cleanup bool) (bool, error) {
	if err := c.fetchDeployment(); client.IgnoreNotFound(err) != nil {
		return false, err
	}

	if _, err := c.releaseDeployment(c.deployment, cleanup); err != nil {
		return false, err
	}

	c.EventRecorder.Eventf(c.Release, v1.EventTypeNormal, "FinalizedSuccessfully", "Rollout resource are finalized: cleanup=%v", cleanup)
	return true, nil
}

// SyncWorkloadInfo return change type if workload was changed during release
func (c *DeploymentInPlaceController) SyncWorkloadInfo() (WorkloadEventType, *util.WorkloadInfo, error) {
	// ignore the sync if the release plan is deleted
	if c.Release.DeletionTimestamp != nil {
		return IgnoreWorkloadEvent, nil, nil
	}

	if err := c.fetchDeployment(); err != nil {
		if apierrors.IsNotFound(err) {
			return WorkloadHasGone, nil, err
		}
		return "", nil, err
	}

	if c.deployment.Status.ObservedGeneration < c.deployment.Generation {
		return WorkloadStillReconciling, nil, nil
	}

	workloadInfo := &util.WorkloadInfo{Status: &util.WorkloadStatus{}}
	newRS, _, err := c.GetNewAndOldReplicaSets(c.deployment)
	if err == nil && newRS != nil {
		workloadInfo.Status.UpdatedReplicas = newRS.Status.Replicas
		workloadInfo.Status.UpdatedReadyReplicas = newRS.Status.AvailableReplicas
	}

	// in case of that the updated revision of the workload is promoted
	if c.deployment.Status.UpdatedReplicas == c.deployment.Status.Replicas {
		return IgnoreWorkloadEvent, workloadInfo, nil
	}
	return IgnoreWorkloadEvent, workloadInfo, nil
}

/* ----------------------------------
The functions below are helper functions
------------------------------------- */
// fetchdeployment fetch deployment to c.clone
func (c *DeploymentInPlaceController) fetchDeployment() error {
	deployment := &appsv1.Deployment{}
	if err := c.Client.Get(context.TODO(), c.deploymentKey, deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			c.EventRecorder.Event(c.Release, v1.EventTypeWarning, "GetdeploymentFailed", err.Error())
		}
		return err
	}
	c.deployment = deployment
	return nil
}

func (c *DeploymentInPlaceController) recordDeploymentRevisionAndReplicas() error {
	var stableRevision string
	if s, ok := c.Release.Labels[util.RolloutStableRevisionLabel]; ok && s != "" {
		stableRevision = s
	} else {
		stableRS, err := util.NewControllerFinder(c.Client).GetDeploymentStableRs(c.deployment)
		if err != nil {
			return err
		}
		if stableRS != nil {
			stableRevision = stableRS.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
		}
	}
	c.newStatus.StableRevision = stableRevision
	c.newStatus.UpdateRevision = util.ComputeHash(&c.deployment.Spec.Template, nil)
	c.newStatus.ObservedWorkloadReplicas = *c.deployment.Spec.Replicas
	return nil
}

func (c *DeploymentInPlaceController) patchPodBatchLabel(canaryGoal int32) (bool, error) {
	rolloutID, exist := c.Release.Labels[util.RolloutIDLabel]
	if !exist || rolloutID == "" {
		return true, nil
	}

	pods, err := util.ListOwnedPods(c.Client, c.deployment)
	if err != nil {
		klog.Errorf("Failed to list pods for deployment %v", c.deploymentKey)
		return false, err
	}

	batchID := c.Release.Status.CanaryStatus.CurrentBatch + 1
	updateRevision := c.Release.Status.UpdateRevision
	return util.PatchPodBatchLabel(c.Client, pods, rolloutID, batchID, updateRevision, canaryGoal, client.ObjectKeyFromObject(c.Release))
}

// add the parent controller to the owner of the deployment, unpause it and initialize the size
// before kicking start the update and start from every pod in the old version
func (c *DeploymentInPlaceController) claimDeployment(deployment *appsv1.Deployment) (bool, error) {
	var controlled bool
	if controlInfo, ok := deployment.Annotations[util.BatchReleaseControlAnnotation]; ok && controlInfo != "" {
		ref := &metav1.OwnerReference{}
		err := json.Unmarshal([]byte(controlInfo), ref)
		if err == nil && ref.UID == c.Release.UID {
			controlled = true
			klog.V(3).Infof("deployment(%v) has been controlled by this BatchRelease(%v), no need to claim again",
				klog.KObj(deployment), klog.KObj(c.Release))
		} else {
			klog.Errorf("Failed to parse controller info from deployment(%v) annotation, error: %v, controller info: %+v",
				klog.KObj(deployment), err, *ref)
		}
	}

	if controlled {
		return true, nil
	}

	strategy, _ := json.Marshal(&deployment.Spec.Strategy)
	controlInfo, _ := json.Marshal(metav1.NewControllerRef(c.Release, c.Release.GroupVersionKind()))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fetched := &appsv1.Deployment{}
		getErr := c.Client.Get(context.TODO(), client.ObjectKeyFromObject(deployment), fetched)
		if getErr != nil {
			return getErr
		}
		fetched.Spec.Paused = true
		fetched.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}
		fetched.Annotations[util.DeploymentPausedAnnotation] = "false"
		fetched.Annotations[util.DeploymentRolloutStrategy] = "inplace"
		fetched.Annotations[util.DeploymentStrategy] = string(strategy)
		fetched.Annotations[util.BatchReleaseControlAnnotation] = string(controlInfo)
		return c.Client.Update(context.TODO(), fetched)
	})
	if err != nil {
		klog.Errorf("Failed to update deployment(%v) when claim it, err: %v", klog.KObj(deployment), err)
	}

	klog.V(3).Infof("Claim Deployment(%v) Successfully", klog.KObj(c.deployment))
	return true, nil
}

// remove the parent controller from the deployment's owner list
func (c *DeploymentInPlaceController) releaseDeployment(deployment *appsv1.Deployment, cleanup bool) (bool, error) {
	if deployment == nil {
		return true, nil
	}

	var found bool
	var refByte string
	if refByte, found = deployment.Annotations[util.BatchReleaseControlAnnotation]; found && refByte != "" {
		ref := &metav1.OwnerReference{}
		if err := json.Unmarshal([]byte(refByte), ref); err != nil {
			found = false
			klog.Errorf("failed to decode controller annotations of BatchRelease")
		} else if ref.UID != c.Release.UID {
			found = false
		}
	}

	if !found {
		klog.V(3).Infof("the deployment(%v) is already released", c.deploymentKey)
		return true, nil
	}

	strategyInfo := deployment.Annotations[util.DeploymentStrategy]
	strategy := appsv1.DeploymentStrategy{}
	if err := json.Unmarshal([]byte(strategyInfo), &strategy); err != nil {
		return false, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fetched := &appsv1.Deployment{}
		getErr := c.Client.Get(context.TODO(), client.ObjectKeyFromObject(deployment), fetched)
		if getErr != nil {
			return getErr
		}
		fetched.Spec.Paused = false
		fetched.Spec.Strategy = *strategy.DeepCopy()
		delete(fetched.Annotations, util.DeploymentStrategy)
		delete(fetched.Annotations, util.DeploymentRolloutStrategy)
		delete(fetched.Annotations, util.BatchReleaseControlAnnotation)
		return c.Client.Update(context.TODO(), fetched)
	})
	if err != nil {
		klog.Errorf("Failed to update deployment(%v) when claim it, err: %v", klog.KObj(deployment), err)
	}

	klog.V(3).Infof("Release deployment(%v) Successfully", c.deploymentKey)
	return true, nil
}
