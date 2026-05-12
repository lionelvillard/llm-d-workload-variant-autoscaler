/*
Copyright 2025 The llm-d Authors

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

package utils

import (
	"github.com/go-logr/logr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// CheckCRDInstalled reports whether a CRD with the given groupVersion and kind is
// registered in the cluster. It is called once at startup; dynamic CRD installation
// after the controller starts is not yet handled.
func CheckCRDInstalled(restConfig *rest.Config, groupVersion, kind string, logger logr.Logger) bool {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		logger.Error(err, "failed to create discovery client for CRD detection",
			"groupVersion", groupVersion, "kind", kind)
		return false
	}

	_, apiLists, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		// Partial errors are common (e.g. unavailable API services); continue if we got results.
		if apiLists == nil {
			logger.Error(err, "failed to discover API resources",
				"groupVersion", groupVersion, "kind", kind)
			return false
		}
		logger.V(1).Info("partial error discovering API resources (this is usually fine)", "error", err)
	}

	for _, apiList := range apiLists {
		if apiList.GroupVersion == groupVersion {
			for _, resource := range apiList.APIResources {
				if resource.Kind == kind {
					return true
				}
			}
		}
	}

	return false
}

// CheckKEDACRD reports whether the KEDA ScaledObject CRD is installed.
// TODO: checked once at startup; handle KEDA installed after controller starts.
func CheckKEDACRD(restConfig *rest.Config, logger logr.Logger) bool {
	return CheckCRDInstalled(restConfig, "keda.sh/v1alpha1", "ScaledObject", logger)
}

// CheckLeaderWorkerSetCRD reports whether the LeaderWorkerSet CRD is installed.
// TODO: checked once at startup; handle LWS installed after controller starts.
func CheckLeaderWorkerSetCRD(restConfig *rest.Config, logger logr.Logger) bool {
	return CheckCRDInstalled(restConfig, "leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", logger)
}
