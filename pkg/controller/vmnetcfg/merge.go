package vmnetcfg

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var errOwnedStateChanged = errors.New("VMNetCfg ownership or owned entries changed during reconciliation")

// ownedConfig is a private working projection. Stored foreign rows never pass
// through canonicalization; API writes merge against the untouched live object.
func (c *Controller) ownedConfig(obj *kihv1.VirtualMachineNetworkConfig) *kihv1.VirtualMachineNetworkConfig {
	owned := *obj
	owned.Spec.NetworkConfig = c.scope.FilterSpec(obj.Namespace, owned.Spec.NetworkConfig)
	owned.Status.NetworkConfig = c.scope.FilterStatus(obj.Namespace, owned.Status.NetworkConfig)
	for i := range owned.Spec.NetworkConfig {
		owned.Spec.NetworkConfig[i].NetworkName = c.scope.NetworkName()
		owned.Spec.NetworkConfig[i].MACAddress = util.CanonicalHWAddr(owned.Spec.NetworkConfig[i].MACAddress)
	}
	for i := range owned.Status.NetworkConfig {
		owned.Status.NetworkConfig[i].NetworkName = c.scope.NetworkName()
		owned.Status.NetworkConfig[i].MACAddress = util.CanonicalHWAddr(owned.Status.NetworkConfig[i].MACAddress)
	}
	return &owned
}

func sameOwnerState(base, live *kihv1.VirtualMachineNetworkConfig) bool {
	return base.UID == live.UID && base.Spec.VMName == live.Spec.VMName &&
		(base.DeletionTimestamp == nil) == (live.DeletionTimestamp == nil) &&
		reflect.DeepEqual(base.OwnerReferences, live.OwnerReferences)
}

func (c *Controller) sameOwnedSpec(base, live *kihv1.VirtualMachineNetworkConfig) bool {
	return reflect.DeepEqual(c.scope.FilterSpec(base.Namespace, base.Spec.NetworkConfig), c.scope.FilterSpec(live.Namespace, live.Spec.NetworkConfig))
}

// retryOwnedWrite repeats API intent only. No allocator or ledger mutation may
// be placed in mutate: conflicts must not allocate twice or replay cleanup.
func (c *Controller) retryOwnedWrite(base *kihv1.VirtualMachineNetworkConfig, status bool, mutate func(*kihv1.VirtualMachineNetworkConfig) (bool, error)) (*kihv1.VirtualMachineNetworkConfig, error) {
	client := c.kihClientset.KubevirtiphelperV1().VirtualMachineNetworkConfigs(base.Namespace)
	for attempt := range 10 {
		live, err := client.Get(c.ctx, base.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if !sameOwnerState(base, live) {
			return nil, errOwnedStateChanged
		}
		changed, err := mutate(live)
		if err != nil {
			return nil, err
		}
		if !changed {
			return live, nil
		}
		var saved *kihv1.VirtualMachineNetworkConfig
		if status {
			saved, err = client.UpdateStatus(c.ctx, live, metav1.UpdateOptions{})
		} else {
			saved, err = client.Update(c.ctx, live, metav1.UpdateOptions{})
		}
		if err == nil {
			return saved, nil
		}
		if !apierrors.IsConflict(err) || attempt == 9 {
			return nil, err
		}
		select {
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("VMNetCfg write retries exhausted")
}

func (c *Controller) commitSpec(base *kihv1.VirtualMachineNetworkConfig, replacement []kihv1.NetworkConfig) (*kihv1.VirtualMachineNetworkConfig, error) {
	return c.retryOwnedWrite(base, false, func(live *kihv1.VirtualMachineNetworkConfig) (bool, error) {
		if !c.sameOwnedSpec(base, live) {
			return false, errOwnedStateChanged
		}
		merged := c.scope.MergeSpec(live.Namespace, live.Spec.NetworkConfig, replacement)
		if reflect.DeepEqual(live.Spec.NetworkConfig, merged) {
			return false, nil
		}
		live.Spec.NetworkConfig = merged
		return true, nil
	})
}

func (c *Controller) commitStatus(base *kihv1.VirtualMachineNetworkConfig, replacement []kihv1.NetworkConfigStatus) (*kihv1.VirtualMachineNetworkConfig, error) {
	return c.retryOwnedWrite(base, true, func(live *kihv1.VirtualMachineNetworkConfig) (bool, error) {
		if !c.sameOwnedSpec(base, live) || !reflect.DeepEqual(c.scope.FilterStatus(base.Namespace, base.Status.NetworkConfig), c.scope.FilterStatus(live.Namespace, live.Status.NetworkConfig)) {
			return false, errOwnedStateChanged
		}
		merged := c.scope.MergeStatus(live.Namespace, live.Status.NetworkConfig, replacement)
		if reflect.DeepEqual(live.Status.NetworkConfig, merged) {
			return false, nil
		}
		live.Status.NetworkConfig = merged
		return true, nil
	})
}
