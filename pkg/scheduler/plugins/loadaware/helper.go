/*
Copyright 2022 The Koordinator Authors.

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

package loadaware

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/koordinator-sh/koordinator/apis/extension"
	slov1alpha1 "github.com/koordinator-sh/koordinator/apis/slo/v1alpha1"
	schedulingconfig "github.com/koordinator-sh/koordinator/pkg/scheduler/apis/config"
	"github.com/koordinator-sh/koordinator/pkg/util"
)

func isNodeMetricExpired(nodeMetric *slov1alpha1.NodeMetric, nodeMetricExpirationSeconds int64) bool {
	return nodeMetric == nil ||
		nodeMetric.Status.UpdateTime == nil ||
		nodeMetricExpirationSeconds > 0 &&
			time.Since(nodeMetric.Status.UpdateTime.Time) >= time.Duration(nodeMetricExpirationSeconds)*time.Second
}

func getNodeMetricReportInterval(nodeMetric *slov1alpha1.NodeMetric) time.Duration {
	if nodeMetric.Spec.CollectPolicy == nil || nodeMetric.Spec.CollectPolicy.ReportIntervalSeconds == nil {
		return DefaultNodeMetricReportInterval
	}
	return time.Duration(*nodeMetric.Spec.CollectPolicy.ReportIntervalSeconds) * time.Second
}

func missedLatestUpdateTime(assignedTime, updateTime time.Time) bool {
	return assignedTime.After(updateTime)
}

func stillInTheReportInterval(assignedTime, updateTime time.Time, reportInterval time.Duration) bool {
	return assignedTime.Before(updateTime) && updateTime.Sub(assignedTime) < reportInterval
}

func getTargetAggregatedUsage(nodeMetric *slov1alpha1.NodeMetric, aggregatedDuration *metav1.Duration, aggregationType extension.AggregationType) *slov1alpha1.ResourceMap {
	if nodeMetric.Status.NodeMetric == nil || len(nodeMetric.Status.NodeMetric.AggregatedNodeUsages) == 0 {
		return nil
	}

	// If no specific period is set, the non-empty maximum period recorded by NodeMetrics will be used by default.
	// This is a default policy.
	if aggregatedDuration == nil || aggregatedDuration.Duration == 0 {
		var maxDuration time.Duration
		var maxIndex int = -1
		for i, v := range nodeMetric.Status.NodeMetric.AggregatedNodeUsages {
			if len(v.Usage[aggregationType].ResourceList) > 0 && v.Duration.Duration > maxDuration {
				maxDuration = v.Duration.Duration
				maxIndex = i
			}
		}

		if maxIndex == -1 {
			// All values in aggregatedDuration are empty, downgrade to use the values in NodeUsage
			usage := nodeMetric.Status.NodeMetric.NodeUsage
			if len(usage.ResourceList) > 0 {
				return &usage
			}
		} else {
			usage := nodeMetric.Status.NodeMetric.AggregatedNodeUsages[maxIndex].Usage[aggregationType]
			return &usage
		}
	} else if aggregatedDuration != nil {
		for _, v := range nodeMetric.Status.NodeMetric.AggregatedNodeUsages {
			if v.Duration.Duration == aggregatedDuration.Duration {
				usage := v.Usage[aggregationType]
				if len(usage.ResourceList) > 0 {
					return &usage
				}
			}
		}
	}
	return nil
}

func filterWithAggregation(args *schedulingconfig.LoadAwareSchedulingAggregatedArgs) bool {
	return args != nil && len(args.UsageThresholds) > 0 && args.UsageAggregationType != ""
}

func scoreWithAggregation(args *schedulingconfig.LoadAwareSchedulingAggregatedArgs) bool {
	return args != nil && args.ScoreAggregationType != ""
}

type usageThresholdsFilterProfile = extension.CustomUsageThresholds

func generateUsageThresholdsFilterProfile(node *corev1.Node, args *schedulingconfig.LoadAwareSchedulingArgs) *usageThresholdsFilterProfile {
	usageThresholds, prodUsageThresholds := args.UsageThresholds, args.ProdUsageThresholds
	customUsageThresholds, err := extension.GetCustomUsageThresholds(node)
	if err != nil {
		klog.V(5).ErrorS(err, "failed to GetCustomUsageThresholds from", "node", node.Name)
		customUsageThresholds = &extension.CustomUsageThresholds{
			UsageThresholds:     usageThresholds,
			ProdUsageThresholds: prodUsageThresholds,
		}
		if filterWithAggregation(args.Aggregated) {
			customUsageThresholds.AggregatedUsage = &extension.CustomAggregatedUsage{
				UsageThresholds:         args.Aggregated.UsageThresholds,
				UsageAggregationType:    args.Aggregated.UsageAggregationType,
				UsageAggregatedDuration: &args.Aggregated.UsageAggregatedDuration,
			}
		}
	} else {
		if len(customUsageThresholds.UsageThresholds) == 0 {
			customUsageThresholds.UsageThresholds = usageThresholds
		}
		if len(customUsageThresholds.ProdUsageThresholds) == 0 {
			customUsageThresholds.ProdUsageThresholds = prodUsageThresholds
		}
		if customUsageThresholds.AggregatedUsage != nil {
			if len(customUsageThresholds.AggregatedUsage.UsageThresholds) == 0 ||
				customUsageThresholds.AggregatedUsage.UsageAggregationType == "" {
				customUsageThresholds.AggregatedUsage = nil
			}
		}
		if customUsageThresholds.AggregatedUsage == nil && filterWithAggregation(args.Aggregated) {
			customUsageThresholds.AggregatedUsage = &extension.CustomAggregatedUsage{
				UsageThresholds:         args.Aggregated.UsageThresholds,
				UsageAggregationType:    args.Aggregated.UsageAggregationType,
				UsageAggregatedDuration: &args.Aggregated.UsageAggregatedDuration,
			}
		}
	}
	return customUsageThresholds
}

func getResourceValue(resourceName corev1.ResourceName, quantity resource.Quantity) int64 {
	if resourceName == corev1.ResourceCPU {
		return quantity.MilliValue()
	}
	return quantity.Value()
}

func buildPodMetricMap(nodeMetric *slov1alpha1.NodeMetric, filterProdPod bool) map[types.NamespacedName]corev1.ResourceList {
	if len(nodeMetric.Status.PodsMetric) == 0 {
		return nil
	}
	podMetrics := make(map[types.NamespacedName]corev1.ResourceList)
	for _, podMetric := range nodeMetric.Status.PodsMetric {
		if filterProdPod && podMetric.Priority != extension.PriorityProd {
			continue
		}
		name := types.NamespacedName{
			Namespace: podMetric.Namespace,
			Name:      podMetric.Name,
		}
		podMetrics[name] = podMetric.PodUsage.ResourceList
	}
	return podMetrics
}

func sumPodUsages(podMetrics map[types.NamespacedName]corev1.ResourceList, estimatedPods sets.Set[types.NamespacedName]) (podUsages, estimatedPodsUsages corev1.ResourceList) {
	if len(podMetrics) == 0 {
		return nil, nil
	}
	podUsages = make(corev1.ResourceList)
	estimatedPodsUsages = make(corev1.ResourceList)
	for podName, usage := range podMetrics {
		if estimatedPods.Has(podName) {
			util.AddResourceList(estimatedPodsUsages, usage)
			continue
		}
		util.AddResourceList(podUsages, usage)
	}
	return podUsages, estimatedPodsUsages
}

// isDaemonSetPod returns true if the pod is a IsDaemonSetPod.
func isDaemonSetPod(ownerRefList []metav1.OwnerReference) bool {
	for _, ownerRef := range ownerRefList {
		if ownerRef.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}
