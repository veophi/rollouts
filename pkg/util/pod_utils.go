package util

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *v1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.PodReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

func IsConsistentWithRevision(pod *v1.Pod, revision string) bool {
	if strings.HasSuffix(pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey], revision) {
		return true
	}
	if strings.HasSuffix(pod.Labels[appsv1.ControllerRevisionHashLabelKey], revision) {
		return true
	}
	return false
}

func IsMatchedRolloutID(pod *v1.Pod, rolloutID string) bool {
	return pod.Labels[RolloutIDLabel] == rolloutID
}

func ListOwnedPods(c client.Client, object client.Object) ([]*v1.Pod, error) {
	selector, err := ParseSelector(object)
	if err != nil {
		return nil, err
	}

	podLister := &v1.PodList{}
	err = c.List(context.TODO(), podLister, &client.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	pods := make([]*v1.Pod, 0)
	for i := range podLister.Items {
		pod := &podLister.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != object.GetUID() {
			continue
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

func PatchPodBatchLabel(c client.Client, pods []*v1.Pod, rolloutID string, batchID int32, updateRevision string, canaryGoal int32, logKey types.NamespacedName) (bool, error) {
	patchedCount := int32(0)
	for _, pod := range pods {
		if !IsConsistentWithRevision(pod, updateRevision) {
			continue
		}
		if pod.Labels[RolloutIDLabel] == rolloutID {
			patchedCount++
		}
	}

	klog.V(3).Infof("Patch %v pods with batchID for batchRelease %v, goal is %d pods", patchedCount, logKey, canaryGoal)

	if patchedCount >= canaryGoal {
		return true, nil
	}

	for _, pod := range pods {
		if !IsMatchedRolloutID(pod, updateRevision) {
			continue
		}

		podRolloutID, exists := pod.Labels[RolloutIDLabel]
		if podRolloutID == rolloutID || (exists && patchedCount >= canaryGoal) {
			continue
		}

		podClone := pod.DeepCopy()
		by := fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s","%s":"%d"}}}`, RolloutIDLabel, rolloutID, RolloutBatchIDLabel, batchID)
		err := c.Patch(context.TODO(), podClone, client.RawPatch(types.StrategicMergePatchType, []byte(by)))
		if err != nil {
			klog.Errorf("Failed to patch Pod(%v) batchID, err: %v", client.ObjectKeyFromObject(podClone), err)
			return false, err
		}
		patchedCount++
	}

	return patchedCount >= canaryGoal, nil
}
