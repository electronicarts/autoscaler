/*
Copyright 2016 The Kubernetes Authors.

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

package core

import (
	"time"

	"k8s.io/autoscaler/cluster-autoscaler/clusterstate/utils"
	"k8s.io/autoscaler/cluster-autoscaler/metrics"
	"k8s.io/autoscaler/cluster-autoscaler/simulator"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	kube_util "k8s.io/autoscaler/cluster-autoscaler/utils/kubernetes"
	kube_client "k8s.io/client-go/kubernetes"
	kube_record "k8s.io/client-go/tools/record"

	"github.com/golang/glog"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
)

// StaticAutoscaler is an autoscaler which has all the core functionality of a CA but without the reconfiguration feature
type StaticAutoscaler struct {
	// AutoscalingContext consists of validated settings and options for this autoscaler
	*AutoscalingContext
	kube_util.ListerRegistry
	lastScaleUpTime          time.Time
	lastScaleDownFailedTrial time.Time
	scaleDown                *ScaleDown
}

// NewStaticAutoscaler creates an instance of Autoscaler filled with provided parameters
func NewStaticAutoscaler(opts AutoscalingOptions, predicateChecker *simulator.PredicateChecker,
	kubeClient kube_client.Interface, kubeEventRecorder kube_record.EventRecorder, listerRegistry kube_util.ListerRegistry) (*StaticAutoscaler, errors.AutoscalerError) {
	logRecorder, err := utils.NewStatusMapRecorder(kubeClient, opts.ConfigNamespace, opts.NamespaceFilter, kubeEventRecorder, opts.WriteStatusConfigMap)
	if err != nil {
		glog.Error("Failed to initialize status configmap, unable to write status events")
		// Get a dummy, so we can at least safely call the methods
		// TODO(maciekpytel): recover from this after successfull status configmap update?
		logRecorder, _ = utils.NewStatusMapRecorder(kubeClient, opts.ConfigNamespace, opts.NamespaceFilter, kubeEventRecorder, false)
	}
	autoscalingContext, errctx := NewAutoscalingContext(opts, predicateChecker, kubeClient, kubeEventRecorder, logRecorder, listerRegistry)
	if errctx != nil {
		return nil, errctx
	}

	scaleDown := NewScaleDown(autoscalingContext)

	return &StaticAutoscaler{
		AutoscalingContext:       autoscalingContext,
		ListerRegistry:           listerRegistry,
		lastScaleUpTime:          time.Now(),
		lastScaleDownFailedTrial: time.Now(),
		scaleDown:                scaleDown,
	}, nil
}

// CleanUp cleans up ToBeDeleted taints added by the previously run and then failed CA
func (a *StaticAutoscaler) CleanUp() {
	// CA can die at any time. Removing taints that might have been left from the previous run.
	if readyNodes, err := a.ReadyNodeLister().List(); err != nil {
		cleanToBeDeleted(readyNodes, a.AutoscalingContext.ClientSet, a.Recorder)
	}
}

// CloudProvider returns the cloud provider associated to this autoscaler
func (a *StaticAutoscaler) CloudProvider() cloudprovider.CloudProvider {
	return a.AutoscalingContext.CloudProvider
}

// RunOnce iterates over node groups and scales them up/down if necessary
func (a *StaticAutoscaler) RunOnce(currentTime time.Time) errors.AutoscalerError {
	readyNodeLister := a.ReadyNodeLister()
	allNodeLister := a.AllNodeLister()
	unschedulablePodLister := a.UnschedulablePodLister()
	scheduledPodLister := a.ScheduledPodLister()
	pdbLister := a.PodDisruptionBudgetLister()
	scaleDown := a.scaleDown
	autoscalingContext := a.AutoscalingContext
	runStart := time.Now()

	readyNodes, err := readyNodeLister.List()
	if err != nil {
		glog.Errorf("Failed to list ready nodes: %v", err)
		return errors.ToAutoscalerError(errors.ApiCallError, err)
	}
	if len(readyNodes) == 0 {
		glog.Warningf("No ready nodes in the cluster")
		scaleDown.CleanUpUnneededNodes()
		return nil
	}

	allNodes, err := allNodeLister.List()
	if err != nil {
		glog.Errorf("Failed to list all nodes: %v", err)
		return errors.ToAutoscalerError(errors.ApiCallError, err)
	}
	if len(allNodes) == 0 {
		glog.Warningf("No nodes in the cluster")
		scaleDown.CleanUpUnneededNodes()
		return nil
	}

	err = a.ClusterStateRegistry.UpdateNodes(allNodes, currentTime)
	if err != nil {
		glog.Errorf("Failed to update node registry: %v", err)
		scaleDown.CleanUpUnneededNodes()
		return errors.ToAutoscalerError(errors.CloudProviderError, err)
	}
	metrics.UpdateClusterState(a.ClusterStateRegistry)

	// Update status information when the loop is done (regardless of reason)
	defer func() {
		if autoscalingContext.WriteStatusConfigMap {
			status := a.ClusterStateRegistry.GetStatus(time.Now())
			utils.WriteStatusConfigMap(autoscalingContext.ClientSet, autoscalingContext.ConfigNamespace,
				autoscalingContext.NamespaceFilter, status.GetReadableString(),
				a.AutoscalingContext.LogRecorder)
		}
	}()
	if !a.ClusterStateRegistry.IsClusterHealthy() {
		glog.Warning("Cluster is not ready for autoscaling")
		scaleDown.CleanUpUnneededNodes()
		return nil
	}

	metrics.UpdateDurationFromStart(metrics.UpdateState, runStart)
	metrics.UpdateLastTime(metrics.Autoscaling, time.Now())

	// Check if there are any nodes that failed to register in Kubernetes
	// master.
	unregisteredNodes := a.ClusterStateRegistry.GetUnregisteredNodes()
	if len(unregisteredNodes) > 0 {
		glog.V(1).Infof("%d unregistered nodes present", len(unregisteredNodes))
		removedAny, err := removeOldUnregisteredNodes(unregisteredNodes, autoscalingContext, time.Now())
		// There was a problem with removing unregistered nodes. Retry in the next loop.
		if err != nil {
			if removedAny {
				glog.Warningf("Some unregistered nodes were removed, but got error: %v", err)
			} else {
				glog.Errorf("Failed to remove unregistered nodes: %v", err)

			}
			return errors.ToAutoscalerError(errors.CloudProviderError, err)
		}
		// Some nodes were removed. Let's skip this iteration, the next one should be better.
		if removedAny {
			glog.V(0).Infof("Some unregistered nodes were removed, skipping iteration")
			return nil
		}
	}

	// Check if there has been a constant difference between the number of nodes in k8s and
	// the number of nodes on the cloud provider side.
	// TODO: andrewskim - add protection for ready AWS nodes.
	fixedSomething, err := fixNodeGroupSize(autoscalingContext, time.Now())
	if err != nil {
		glog.Errorf("Failed to fix node group sizes: %v", err)
		return errors.ToAutoscalerError(errors.CloudProviderError, err)
	}
	if fixedSomething {
		glog.V(0).Infof("Some node group target size was fixed, skipping the iteration")
		return nil
	}

	allUnschedulablePods, err := unschedulablePodLister.List()
	if err != nil {
		glog.Errorf("Failed to list unscheduled pods: %v", err)
		return errors.ToAutoscalerError(errors.ApiCallError, err)
	}
	metrics.UpdateUnschedulablePodsCount(len(allUnschedulablePods))

	allScheduled, err := scheduledPodLister.List()
	if err != nil {
		glog.Errorf("Failed to list scheduled pods: %v", err)
		return errors.ToAutoscalerError(errors.ApiCallError, err)
	}

	ConfigurePredicateCheckerForLoop(allUnschedulablePods, allScheduled, a.PredicateChecker)

	// We need to check whether pods marked as unschedulable are actually unschedulable.
	// It's likely we added a new node and the scheduler just haven't managed to put the
	// pod on in yet. In this situation we don't want to trigger another scale-up.
	//
	// It's also important to prevent uncontrollable cluster growth if CA's simulated
	// scheduler differs in opinion with real scheduler. Example of such situation:
	// - CA and Scheduler has slightly different configuration
	// - Scheduler can't schedule a pod and marks it as unschedulable
	// - CA added a node which should help the pod
	// - Scheduler doesn't schedule the pod on the new node
	//   because according to it logic it doesn't fit there
	// - CA see the pod is still unschedulable, so it adds another node to help it
	//
	// With the check enabled the last point won't happen because CA will ignore a pod
	// which is supposed to schedule on an existing node.
	schedulablePodsPresent := false

	glog.V(4).Infof("Filtering out schedulables")
	filterOutSchedulableStart := time.Now()
	unschedulablePodsToHelp := FilterOutSchedulable(allUnschedulablePods, readyNodes, allScheduled,
		a.PredicateChecker)
	metrics.UpdateDurationFromStart(metrics.FilterOutSchedulable, filterOutSchedulableStart)

	if len(unschedulablePodsToHelp) != len(allUnschedulablePods) {
		glog.V(2).Info("Schedulable pods present")
		schedulablePodsPresent = true
	} else {
		glog.V(4).Info("No schedulable pods")
	}

	if len(unschedulablePodsToHelp) == 0 {
		glog.V(1).Info("No unschedulable pods")
	} else if a.MaxNodesTotal > 0 && len(readyNodes) >= a.MaxNodesTotal {
		glog.V(1).Info("Max total nodes in cluster reached")
	} else {
		daemonsets, err := a.ListerRegistry.DaemonSetLister().List()
		if err != nil {
			glog.Errorf("Failed to get daemonset list")
			return errors.ToAutoscalerError(errors.ApiCallError, err)
		}

		scaleUpStart := time.Now()
		metrics.UpdateLastTime(metrics.ScaleUp, scaleUpStart)

		scaledUp, typedErr := ScaleUp(autoscalingContext, unschedulablePodsToHelp, readyNodes, daemonsets)

		metrics.UpdateDurationFromStart(metrics.ScaleUp, scaleUpStart)

		if typedErr != nil {
			glog.Errorf("Failed to scale up: %v", typedErr)
			return typedErr
		} else if scaledUp {
			a.lastScaleUpTime = time.Now()
			// No scale down in this iteration.
			return nil
		}
	}

	if a.ScaleDownEnabled {
		pdbs, err := pdbLister.List()
		if err != nil {
			glog.Errorf("Failed to list pod disruption budgets: %v", err)
			return errors.ToAutoscalerError(errors.ApiCallError, err)
		}

		unneededStart := time.Now()
		// In dry run only utilization is updated
		calculateUnneededOnly := a.lastScaleUpTime.Add(a.ScaleDownDelay).After(time.Now()) ||
			a.lastScaleDownFailedTrial.Add(a.ScaleDownTrialInterval).After(time.Now()) ||
			schedulablePodsPresent ||
			scaleDown.nodeDeleteStatus.IsDeleteInProgress()

		glog.V(4).Infof("Scale down status: unneededOnly=%v lastScaleUpTime=%s "+
			"lastScaleDownFailedTrail=%s schedulablePodsPresent=%v", calculateUnneededOnly,
			a.lastScaleUpTime, a.lastScaleDownFailedTrial, schedulablePodsPresent)

		glog.V(4).Infof("Calculating unneeded nodes")

		scaleDown.CleanUp(time.Now())
		potentiallyUnneeded := getPotentiallyUnneededNodes(autoscalingContext, allNodes)

		typedErr := scaleDown.UpdateUnneededNodes(allNodes, potentiallyUnneeded, allScheduled, time.Now(), pdbs)
		if typedErr != nil {
			glog.Errorf("Failed to scale down: %v", typedErr)
			return typedErr
		}

		metrics.UpdateDurationFromStart(metrics.FindUnneeded, unneededStart)

		for key, val := range scaleDown.unneededNodes {
			if glog.V(4) {
				glog.V(4).Infof("%s is unneeded since %s duration %s", key, val.String(), time.Now().Sub(val).String())
			}
		}

		if !calculateUnneededOnly {
			glog.V(4).Infof("Starting scale down")

			// We want to delete unneeded Node Groups only if there was no recent scale up,
			// and there is no current delete in progress and there was no recent errors.
			if a.AutoscalingContext.NodeAutoprovisioningEnabled {
				err := cleanUpNodeAutoprovisionedGroups(a.AutoscalingContext.CloudProvider)
				if err != nil {
					glog.Warningf("Failed to clean up unneded node groups: %v", err)
				}
			}

			scaleDownStart := time.Now()
			metrics.UpdateLastTime(metrics.ScaleDown, scaleDownStart)
			result, typedErr := scaleDown.TryToScaleDown(allNodes, allScheduled, pdbs)
			metrics.UpdateDurationFromStart(metrics.ScaleDown, scaleDownStart)

			// TODO: revisit result handling
			if typedErr != nil {
				glog.Errorf("Failed to scale down: %v", err)
				return typedErr
			}
			if result == ScaleDownError {
				a.lastScaleDownFailedTrial = time.Now()
			}
		}
	}
	return nil
}

// ExitCleanUp removes status configmap.
func (a *StaticAutoscaler) ExitCleanUp() {
	if !a.AutoscalingContext.WriteStatusConfigMap {
		return
	}
	utils.DeleteStatusConfigMap(a.AutoscalingContext.ClientSet, a.AutoscalingContext.ConfigNamespace, a.AutoscalingContext.NamespaceFilter)
}
