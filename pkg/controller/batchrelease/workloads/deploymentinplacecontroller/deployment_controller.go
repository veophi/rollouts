/*
Copyright 2015 The Kubernetes Authors.

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

// Package deployment contains all the logic for handling Kubernetes Deployments.
// It implements a set of strategies (rolling, recreate) for deploying an application,
// the means to rollback to previous versions, proportional scaling for mitigating
// risk, cleanup policy, and other useful features of Deployments.
package deploymentinplacecontroller

import (
	"context"
	"fmt"
	"time"

	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/openkruise/rollouts/api/v1alpha1"
	"github.com/openkruise/rollouts/pkg/controller/batchrelease/workloads/deploymentinplacecontroller/util"
	utilclient "github.com/openkruise/rollouts/pkg/util/client"
)

const (
	// maxRetries is the number of times a deployment will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a deployment is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = apps.SchemeGroupVersion.WithKind("Deployment")

// DeploymentController is responsible for synchronizing Deployment objects stored
// in the system with actual running replica sets and pods.
type DeploymentController struct {
	Client        client.Client
	EventRecorder record.EventRecorder
	Release       *appsv1alpha1.BatchRelease
}

func NewDeploymentController(cli client.Client, recorder record.EventRecorder, release *appsv1alpha1.BatchRelease) *DeploymentController {
	return &DeploymentController{
		Client:        cli,
		Release:       release,
		EventRecorder: recorder,
	}
}

// getReplicaSetsForDeployment uses ControllerRefManager to reconcile
// ControllerRef by adopting and orphaning.
// It returns the list of ReplicaSets that this Deployment should manage.
func (dc *DeploymentController) getReplicaSetsForDeployment(d *apps.Deployment) ([]*apps.ReplicaSet, error) {
	// List all ReplicaSets to find those we own but that no longer match our
	// selector. They will be orphaned by ClaimReplicaSets().
	deploymentSelector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s has invalid label selector: %v", d.Namespace, d.Name, err)
	}
	rsLister := &apps.ReplicaSetList{}
	err = dc.Client.List(context.TODO(), rsLister, &client.ListOptions{Namespace: d.Namespace, LabelSelector: deploymentSelector}, utilclient.DisableDeepCopy)
	if err != nil {
		return nil, err
	}
	rsList := make([]*apps.ReplicaSet, 0, len(rsLister.Items))
	for i := range rsLister.Items {
		rs := &rsLister.Items[i]
		if !rs.DeletionTimestamp.IsZero() {
			continue
		}
		if owner := metav1.GetControllerOfNoCopy(rs); owner == nil || owner.UID != d.UID {
			continue
		}
		rsList = append(rsList, rs)
	}
	return rsList, nil
}

// SyncDeployment will sync the deployment with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (dc *DeploymentController) SyncDeployment(deployment *apps.Deployment) (bool, error) {
	startTime := time.Now()
	klog.V(4).InfoS("Started syncing deployment", "deployment", klog.KObj(deployment), "startTime", startTime)
	defer func() {
		klog.V(4).InfoS("Finished syncing deployment", "deployment", klog.KObj(deployment), "duration", time.Since(startTime))
	}()

	// Deep-copy otherwise we are mutating our cache.
	// TODO: Deep-copy only when needed.
	d := deployment.DeepCopy()

	// List ReplicaSets owned by this Deployment, while reconciling ControllerRef
	// through adoption/orphaning.
	rsList, err := dc.getReplicaSetsForDeployment(d)
	if err != nil {
		return false, err
	}

	if util.DeploymentPaused(d) {
		return dc.sync(context.TODO(), d, rsList)
	}

	scalingEvent, err := dc.isScalingEvent(d, rsList)
	if err != nil {
		return false, err
	}
	if scalingEvent {
		klog.Infof("deployment %v is scaling", klog.KObj(d))
		return dc.sync(context.TODO(), d, rsList)
	}

	return dc.rolloutRolling(context.TODO(), d, rsList)
}

func (dc *DeploymentController) GetNewAndOldReplicaSets(deployment *apps.Deployment) (*apps.ReplicaSet, []*apps.ReplicaSet, error) {
	rsList, err := dc.getReplicaSetsForDeployment(deployment)
	if err != nil {
		return nil, nil, err
	}

	return dc.getAllReplicaSetsAndSyncRevision(deployment, rsList, false)
}

func (dc *DeploymentController) IsBatchReady(deployment *apps.Deployment) (bool, error) {
	if deployment.Status.ObservedGeneration < deployment.Generation ||
		deployment.Status.Replicas != *(deployment.Spec.Replicas) ||
		deployment.Status.AvailableReplicas+util.MaxUnavailable(*deployment) < *deployment.Spec.Replicas {
		return false, nil
	}

	newRS, oldRSs, err := dc.GetNewAndOldReplicaSets(deployment)
	if err != nil {
		return false, err
	}

	if newRS.Status.ObservedGeneration != newRS.Generation {
		return false, nil
	}

	oldReplicas := util.GetReplicaCountForReplicaSets(oldRSs)
	newRSDesired := dc.currentBatchNewReplicaLimit(deployment)
	oldRSDesired := *(deployment.Spec.Replicas) - newRSDesired
	maxUnavailable := util.MaxUnavailableWithReplicas(*deployment, *newRS.Spec.Replicas)

	isNewRSReady := newRSDesired <= *newRS.Spec.Replicas &&
		newRS.Generation <= newRS.Status.ObservedGeneration &&
		*newRS.Spec.Replicas == newRS.Status.Replicas &&
		*newRS.Spec.Replicas <= newRS.Status.AvailableReplicas+maxUnavailable &&
		(newRSDesired == 0 || newRS.Status.AvailableReplicas > 0)

	isOldRSReady := oldRSDesired >= oldReplicas && oldReplicas == util.GetActualReplicaCountForReplicaSets(oldRSs)
	return isNewRSReady && isOldRSReady, nil
}
