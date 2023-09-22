/*
Copyright 2023 The Kubernetes Authors.

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

// Code generated by applyconfiguration-gen. DO NOT EDIT.

package applyconfiguration

import (
	v1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kubevirtiphelperk8sbinbashorgv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/applyconfiguration/kubevirtiphelper.k8s.binbash.org/v1"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
)

// ForKind returns an apply configuration type for the given GroupVersionKind, or nil if no
// apply configuration type exists for the given GroupVersionKind.
func ForKind(kind schema.GroupVersionKind) interface{} {
	switch kind {
	// Group=kubevirtiphelper.k8s.binbash.org, Version=v1
	case v1.SchemeGroupVersion.WithKind("IPPool"):
		return &kubevirtiphelperk8sbinbashorgv1.IPPoolApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("IPPoolSpec"):
		return &kubevirtiphelperk8sbinbashorgv1.IPPoolSpecApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("IPPoolStatus"):
		return &kubevirtiphelperk8sbinbashorgv1.IPPoolStatusApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("IPv4Config"):
		return &kubevirtiphelperk8sbinbashorgv1.IPv4ConfigApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("IPv4Status"):
		return &kubevirtiphelperk8sbinbashorgv1.IPv4StatusApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("NetworkConfig"):
		return &kubevirtiphelperk8sbinbashorgv1.NetworkConfigApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("NetworkConfigStatus"):
		return &kubevirtiphelperk8sbinbashorgv1.NetworkConfigStatusApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("Pool"):
		return &kubevirtiphelperk8sbinbashorgv1.PoolApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("VirtualMachineNetworkConfig"):
		return &kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("VirtualMachineNetworkConfigSpec"):
		return &kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigSpecApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("VirtualMachineNetworkConfigStatus"):
		return &kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigStatusApplyConfiguration{}

	}
	return nil
}