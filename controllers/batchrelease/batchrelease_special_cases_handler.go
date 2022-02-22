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

package batchrelease

import (
	"github.com/openkruise/rollouts/controllers/batchrelease/workloads"
	"time"

	"github.com/openkruise/rollouts/api/v1alpha1"
	"github.com/openkruise/rollouts/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	Keep        = "Keep"
	Abort       = "Abort"
	Start       = "Start"
	Restart     = "Restart"
	Finalize    = "Finalize"
	Terminating = "Terminating"
	Recalculate = "Recalculate"
)

func (r *Executor) checkHealthyBeforeExecution(controller workloads.WorkloadController) (needStopThisRound bool, result reconcile.Result) {
	var reason string
	var message string
	var action string

	// watch the event of workload change
	workloadEvent, workloadInfo, err := controller.SyncWorkloadInfo()

	// Note: must keep the order of the following cases
	switch {
	case r.releasePlanTerminating():
		reason = "PlanTerminating"
		message = "release plan is terminating, terminate the release plan"
		needStopThisRound = false
		action = Terminating

	case r.workloadHasGone(err):
		reason = "WorkloadGone"
		message = "target workload has gone, cancel the release plan"
		needStopThisRound = false
		action = Terminating

	case client.IgnoreNotFound(err) != nil:
		reason = "GetWorkloadError"
		message = err.Error()
		needStopThisRound = true
		action = Keep
		result = reconcile.Result{RequeueAfter: DefaultShortDuration}

	case workloadEvent == workloads.WorkloadReplicasChanged:
		reason = "ReplicasChanged"
		message = "workload is scaling, recalculate the canary batch"
		needStopThisRound = true
		action = Recalculate

	case r.releasePlanPaused():
		reason = "PlanPaused"
		message = "release plan is paused, no need to reconcile"
		needStopThisRound = true
		action = Keep

	case r.releasePlanUnhealthy():
		reason = "PlanStatusUnhealthy"
		message = "release plan status is unhealthy, try to restart the release plan"
		needStopThisRound = true
		action = Restart

	case r.releasePlanChanged():
		reason = "PlanChanged"
		message = "release plan was changed, try to recalculate the canary status"
		needStopThisRound = true
		action = Recalculate

	case r.locatedWorkloadAndStart(err):
		needStopThisRound = false
		action = Start

	case workloadEvent == workloads.WorkloadRollback:
		reason = "StableOrRollback"
		message = "workload is table or rolling back, abort the release plan"
		needStopThisRound = false
		action = Abort

	case workloadEvent == workloads.WorkloadPodTemplateChanged:
		reason = "TargetRevisionChanged"
		// Rollout controller needs route traffic firstly
		if !util.IsControlledByRollout(r.release) {
			message = "workload revision was changed, try to restart the release plan"
			needStopThisRound = true
			action = Restart
		} else {
			message = "workload revision was changed, stop the release plan"
			needStopThisRound = false
			action = Finalize
		}

	case workloadEvent == workloads.WorkloadUnHealthy:
		reason = "WorkloadUnHealthy"
		message = "workload is UnHealthy, should stop the release plan"
		needStopThisRound = true
		action = Keep

	case workloadEvent == workloads.WorkloadStillReconciling:
		if r.releaseStatus.Phase != v1alpha1.RolloutPhaseCompleted {
			reason = "WorkloadNotStable"
			message = "workload status is not stable, wait for it to be stable"
		}
		needStopThisRound = true
		action = Keep

	default:
		// check canary batch pause seconds
		if r.releaseStatus.Phase == v1alpha1.RolloutPhaseProgressing &&
			r.releaseStatus.CanaryStatus.CurrentBatchState == v1alpha1.ReadyBatchState &&
			int(r.releaseStatus.CanaryStatus.CurrentBatch) < len(r.releasePlan.Batches) {
			currentTimestamp := time.Now()
			currentBatch := r.releasePlan.Batches[r.releaseStatus.CanaryStatus.CurrentBatch]
			waitDuration := time.Duration(currentBatch.PauseSeconds) * time.Second
			if waitDuration > 0 && r.releaseStatus.CanaryStatus.BatchReadyTime.Time.Add(waitDuration).After(currentTimestamp) {
				needStopThisRound = true
				restDuration := r.releaseStatus.CanaryStatus.BatchReadyTime.Time.Add(waitDuration).Sub(currentTimestamp)
				result = reconcile.Result{RequeueAfter: restDuration}
				klog.V(3).Infof("BatchRelease %v/%v paused and will continue to reconcile after %v", r.release.Namespace, r.release.Name, restDuration)
			}
		}
	}

	if len(message) > 0 {
		setCondition(r.releaseStatus, reason, message, v1.ConditionFalse)
		r.recorder.Eventf(r.release, v1.EventTypeWarning, reason, message)
		klog.Warningf("Special case occurred in BatchRelease(%v), message: %v", r.releaseKey, message)
	}

	// refresh workload info
	if workloadInfo != nil {
		if workloadInfo.Replicas != nil {
			r.releaseStatus.ObservedWorkloadReplicas = *workloadInfo.Replicas
		}
		if workloadInfo.UpdateRevision != nil {
			r.releaseStatus.UpdateRevision = *workloadInfo.UpdateRevision
		}
		if workloadInfo.Status != nil {
			r.releaseStatus.CanaryStatus.UpdatedReplicas = workloadInfo.Status.UpdatedReplicas
			r.releaseStatus.CanaryStatus.UpdatedReadyReplicas = workloadInfo.Status.UpdatedReadyReplicas
		}
	}

	switch action {
	case Keep:
		// keep current status, do nothing
	case Start:
		signalStart(r.releaseStatus)
	case Abort:
		signalAbort(r.releaseStatus)
	case Terminating:
		signalTerminating(r.releaseStatus)
	case Finalize:
		signalFinalize(r.releaseStatus)
	case Restart:
		signalRestart(r.releaseStatus)
		result = reconcile.Result{RequeueAfter: DefaultShortDuration}
	case Recalculate:
		signalRecalculate(r.releaseStatus)
		result = reconcile.Result{RequeueAfter: DefaultShortDuration}
	}

	return needStopThisRound, result
}

func (r *Executor) releasePlanTerminating() bool {
	return r.isTerminating()
}

func (r *Executor) releasePlanUnhealthy() bool {
	return r.isProgressing() && int(r.release.Status.CanaryStatus.CurrentBatch) >= len(r.releasePlan.Batches)
}

func (r *Executor) releasePlanChanged() bool {
	return r.isProgressing() && r.releaseStatus.ObservedReleasePlanHash != hashReleasePlanBatches(r.releasePlan)
}

func (r *Executor) locatedWorkloadAndStart(err error) bool {
	return err == nil && r.releaseStatus.Phase == v1alpha1.RolloutPhaseInitial
}

func (r *Executor) workloadHasGone(err error) bool {
	return !r.isTerminating() && r.releaseStatus.Phase != v1alpha1.RolloutPhaseInitial && errors.IsNotFound(err)
}

func (r *Executor) releasePlanPaused() bool {
	partitioned := r.releasePlan.BatchPartition != nil &&
		r.releaseStatus.Phase == v1alpha1.RolloutPhaseProgressing &&
		r.releaseStatus.CanaryStatus.CurrentBatchState == v1alpha1.ReadyBatchState &&
		r.releaseStatus.CanaryStatus.CurrentBatch >= *r.releasePlan.BatchPartition
	return !r.isTerminating() && (r.releasePlan.Paused || partitioned)
}

func (r *Executor) isTerminating() bool {
	return r.release.DeletionTimestamp != nil ||
		r.release.Status.Phase == v1alpha1.RolloutPhaseTerminating ||
		(r.release.Spec.Cancelled && r.releaseStatus.Phase != v1alpha1.RolloutPhaseCancelled)

}

func (r *Executor) isProgressing() bool {
	return !r.release.Spec.Cancelled &&
		r.release.DeletionTimestamp != nil &&
		r.releaseStatus.Phase == v1alpha1.RolloutPhaseProgressing
}
