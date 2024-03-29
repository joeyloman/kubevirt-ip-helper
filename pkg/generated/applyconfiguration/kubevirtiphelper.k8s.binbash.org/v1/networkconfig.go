/*
Copyright 2024 The Kubernetes Authors.

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

package v1

// NetworkConfigApplyConfiguration represents an declarative configuration of the NetworkConfig type for use
// with apply.
type NetworkConfigApplyConfiguration struct {
	IPAddress   *string `json:"ipaddress,omitempty"`
	MACAddress  *string `json:"macaddress,omitempty"`
	NetworkName *string `json:"networkname,omitempty"`
}

// NetworkConfigApplyConfiguration constructs an declarative configuration of the NetworkConfig type for use with
// apply.
func NetworkConfig() *NetworkConfigApplyConfiguration {
	return &NetworkConfigApplyConfiguration{}
}

// WithIPAddress sets the IPAddress field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the IPAddress field is set to the value of the last call.
func (b *NetworkConfigApplyConfiguration) WithIPAddress(value string) *NetworkConfigApplyConfiguration {
	b.IPAddress = &value
	return b
}

// WithMACAddress sets the MACAddress field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the MACAddress field is set to the value of the last call.
func (b *NetworkConfigApplyConfiguration) WithMACAddress(value string) *NetworkConfigApplyConfiguration {
	b.MACAddress = &value
	return b
}

// WithNetworkName sets the NetworkName field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the NetworkName field is set to the value of the last call.
func (b *NetworkConfigApplyConfiguration) WithNetworkName(value string) *NetworkConfigApplyConfiguration {
	b.NetworkName = &value
	return b
}
