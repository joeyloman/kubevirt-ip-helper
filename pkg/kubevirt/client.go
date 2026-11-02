// Package kubevirt provides the read-only slice of the KubeVirt API used by
// the helper. kubevirt.io/client-go pins its REST client to the storage
// version, which is still v1alpha3 in the pinned release; a KubeVirt API
// server that serves v1 answers such requests with a deprecation warning,
// so the helper talks to the served v1 group directly.
package kubevirt

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

const virtualMachinesResource = "virtualmachines"

var (
	kubevirtScheme         = runtime.NewScheme()
	kubevirtCodecs         = serializer.NewCodecFactory(kubevirtScheme)
	kubevirtParameterCodec = runtime.NewParameterCodec(kubevirtScheme)
)

func init() {
	utilruntime.Must(kubevirtv1.AddToScheme(kubevirtScheme))
}

// Client is a KubeVirt v1 REST client.
type Client struct {
	rest rest.Interface
}

// NewClient builds a client for the kubevirt.io/v1 API group.
func NewClient(config *rest.Config) (*Client, error) {
	cfg := rest.CopyConfig(config)
	cfg.GroupVersion = &kubevirtv1.GroupVersion
	cfg.APIPath = "/apis"
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: kubevirtCodecs}

	restClient, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{rest: restClient}, nil
}

// VirtualMachineListWatch lists and watches VirtualMachines in all
// namespaces.
func (c *Client) VirtualMachineListWatch() *cache.ListWatch {
	return &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			list := &kubevirtv1.VirtualMachineList{}
			err := c.rest.Get().
				Namespace(metav1.NamespaceAll).
				Resource(virtualMachinesResource).
				VersionedParams(&options, kubevirtParameterCodec).
				Do(context.TODO()).
				Into(list)

			return list, err
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.Watch = true

			return c.rest.Get().
				Namespace(metav1.NamespaceAll).
				Resource(virtualMachinesResource).
				VersionedParams(&options, kubevirtParameterCodec).
				Watch(context.TODO())
		},
	}
}

// VirtualMachine fetches a VirtualMachine by namespace and name.
func (c *Client) VirtualMachine(ctx context.Context, namespace, name string) (*kubevirtv1.VirtualMachine, error) {
	vm := &kubevirtv1.VirtualMachine{}
	err := c.rest.Get().
		Resource(virtualMachinesResource).
		Namespace(namespace).
		Name(name).
		VersionedParams(&metav1.GetOptions{}, kubevirtParameterCodec).
		Do(ctx).
		Into(vm)

	return vm, err
}
