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

// Code generated by client-gen. DO NOT EDIT.

package v1

import (
	"context"
	json "encoding/json"
	"fmt"
	"time"

	v1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kubevirtiphelperk8sbinbashorgv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/applyconfiguration/kubevirtiphelper.k8s.binbash.org/v1"
	scheme "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
)

// VirtualMachineNetworkConfigsGetter has a method to return a VirtualMachineNetworkConfigInterface.
// A group's client should implement this interface.
type VirtualMachineNetworkConfigsGetter interface {
	VirtualMachineNetworkConfigs(namespace string) VirtualMachineNetworkConfigInterface
}

// VirtualMachineNetworkConfigInterface has methods to work with VirtualMachineNetworkConfig resources.
type VirtualMachineNetworkConfigInterface interface {
	Create(ctx context.Context, virtualMachineNetworkConfig *v1.VirtualMachineNetworkConfig, opts metav1.CreateOptions) (*v1.VirtualMachineNetworkConfig, error)
	Update(ctx context.Context, virtualMachineNetworkConfig *v1.VirtualMachineNetworkConfig, opts metav1.UpdateOptions) (*v1.VirtualMachineNetworkConfig, error)
	UpdateStatus(ctx context.Context, virtualMachineNetworkConfig *v1.VirtualMachineNetworkConfig, opts metav1.UpdateOptions) (*v1.VirtualMachineNetworkConfig, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.VirtualMachineNetworkConfig, error)
	List(ctx context.Context, opts metav1.ListOptions) (*v1.VirtualMachineNetworkConfigList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1.VirtualMachineNetworkConfig, err error)
	Apply(ctx context.Context, virtualMachineNetworkConfig *kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigApplyConfiguration, opts metav1.ApplyOptions) (result *v1.VirtualMachineNetworkConfig, err error)
	ApplyStatus(ctx context.Context, virtualMachineNetworkConfig *kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigApplyConfiguration, opts metav1.ApplyOptions) (result *v1.VirtualMachineNetworkConfig, err error)
	VirtualMachineNetworkConfigExpansion
}

// virtualMachineNetworkConfigs implements VirtualMachineNetworkConfigInterface
type virtualMachineNetworkConfigs struct {
	client rest.Interface
	ns     string
}

// newVirtualMachineNetworkConfigs returns a VirtualMachineNetworkConfigs
func newVirtualMachineNetworkConfigs(c *KubevirtiphelperV1Client, namespace string) *virtualMachineNetworkConfigs {
	return &virtualMachineNetworkConfigs{
		client: c.RESTClient(),
		ns:     namespace,
	}
}

// Get takes name of the virtualMachineNetworkConfig, and returns the corresponding virtualMachineNetworkConfig object, and an error if there is any.
func (c *virtualMachineNetworkConfigs) Get(ctx context.Context, name string, options metav1.GetOptions) (result *v1.VirtualMachineNetworkConfig, err error) {
	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do(ctx).
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of VirtualMachineNetworkConfigs that match those selectors.
func (c *virtualMachineNetworkConfigs) List(ctx context.Context, opts metav1.ListOptions) (result *v1.VirtualMachineNetworkConfigList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1.VirtualMachineNetworkConfigList{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do(ctx).
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested virtualMachineNetworkConfigs.
func (c *virtualMachineNetworkConfigs) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch(ctx)
}

// Create takes the representation of a virtualMachineNetworkConfig and creates it.  Returns the server's representation of the virtualMachineNetworkConfig, and an error, if there is any.
func (c *virtualMachineNetworkConfigs) Create(ctx context.Context, virtualMachineNetworkConfig *v1.VirtualMachineNetworkConfig, opts metav1.CreateOptions) (result *v1.VirtualMachineNetworkConfig, err error) {
	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Post().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(virtualMachineNetworkConfig).
		Do(ctx).
		Into(result)
	return
}

// Update takes the representation of a virtualMachineNetworkConfig and updates it. Returns the server's representation of the virtualMachineNetworkConfig, and an error, if there is any.
func (c *virtualMachineNetworkConfigs) Update(ctx context.Context, virtualMachineNetworkConfig *v1.VirtualMachineNetworkConfig, opts metav1.UpdateOptions) (result *v1.VirtualMachineNetworkConfig, err error) {
	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(virtualMachineNetworkConfig.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(virtualMachineNetworkConfig).
		Do(ctx).
		Into(result)
	return
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *virtualMachineNetworkConfigs) UpdateStatus(ctx context.Context, virtualMachineNetworkConfig *v1.VirtualMachineNetworkConfig, opts metav1.UpdateOptions) (result *v1.VirtualMachineNetworkConfig, err error) {
	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(virtualMachineNetworkConfig.Name).
		SubResource("status").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(virtualMachineNetworkConfig).
		Do(ctx).
		Into(result)
	return
}

// Delete takes name of the virtualMachineNetworkConfig and deletes it. Returns an error if one occurs.
func (c *virtualMachineNetworkConfigs) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *virtualMachineNetworkConfigs) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	var timeout time.Duration
	if listOpts.TimeoutSeconds != nil {
		timeout = time.Duration(*listOpts.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		VersionedParams(&listOpts, scheme.ParameterCodec).
		Timeout(timeout).
		Body(&opts).
		Do(ctx).
		Error()
}

// Patch applies the patch and returns the patched virtualMachineNetworkConfig.
func (c *virtualMachineNetworkConfigs) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1.VirtualMachineNetworkConfig, err error) {
	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Patch(pt).
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(name).
		SubResource(subresources...).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}

// Apply takes the given apply declarative configuration, applies it and returns the applied virtualMachineNetworkConfig.
func (c *virtualMachineNetworkConfigs) Apply(ctx context.Context, virtualMachineNetworkConfig *kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigApplyConfiguration, opts metav1.ApplyOptions) (result *v1.VirtualMachineNetworkConfig, err error) {
	if virtualMachineNetworkConfig == nil {
		return nil, fmt.Errorf("virtualMachineNetworkConfig provided to Apply must not be nil")
	}
	patchOpts := opts.ToPatchOptions()
	data, err := json.Marshal(virtualMachineNetworkConfig)
	if err != nil {
		return nil, err
	}
	name := virtualMachineNetworkConfig.Name
	if name == nil {
		return nil, fmt.Errorf("virtualMachineNetworkConfig.Name must be provided to Apply")
	}
	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Patch(types.ApplyPatchType).
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(*name).
		VersionedParams(&patchOpts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}

// ApplyStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating ApplyStatus().
func (c *virtualMachineNetworkConfigs) ApplyStatus(ctx context.Context, virtualMachineNetworkConfig *kubevirtiphelperk8sbinbashorgv1.VirtualMachineNetworkConfigApplyConfiguration, opts metav1.ApplyOptions) (result *v1.VirtualMachineNetworkConfig, err error) {
	if virtualMachineNetworkConfig == nil {
		return nil, fmt.Errorf("virtualMachineNetworkConfig provided to Apply must not be nil")
	}
	patchOpts := opts.ToPatchOptions()
	data, err := json.Marshal(virtualMachineNetworkConfig)
	if err != nil {
		return nil, err
	}

	name := virtualMachineNetworkConfig.Name
	if name == nil {
		return nil, fmt.Errorf("virtualMachineNetworkConfig.Name must be provided to Apply")
	}

	result = &v1.VirtualMachineNetworkConfig{}
	err = c.client.Patch(types.ApplyPatchType).
		Namespace(c.ns).
		Resource("virtualmachinenetworkconfigs").
		Name(*name).
		SubResource("status").
		VersionedParams(&patchOpts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}
