// Package ippoolstatus updates the persisted allocation ledger of an
// IPPool object. The vm and vmnetcfg controllers both record and remove
// their binding allocations in the pool status; the retry handling and the
// owner validation must be one implementation so the two writers cannot
// drift apart.
package ippoolstatus

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	log "github.com/sirupsen/logrus"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"

	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

const (
	maxRetries = 10
	retryDelay = 100 * time.Millisecond
)

// EventAdd and EventDelete are the ledger mutation tokens of UpdateStatus;
// the vm and vmnetcfg controllers alias their own event constants to them,
// so the case labels below can never drift from what the callers send.
const (
	EventAdd    = "add"
	EventDelete = "delete"
)

// UpdateStatus adds (event=ADD) or removes (event=DELETE) the allocation
// entry of one binding in the status ledger of an IPPool, converging
// against concurrent writers through resource-version conflicts and
// honoring the owner validation: an entry which the ledger records for
// another owner is never overwritten nor removed. The decision is made
// against a fresh read on every retry, so a partially applied update of a
// competing writer survives.
// Each fresh read must still name networkName, the caller's canonical
// network, before either owner convergence or a ledger mutation is allowed.
//
// The retry wait observes ctx: a canceled era (application reinit or
// shutdown) aborts the retry instead of blocking the worker for the full
// backoff.
func UpdateStatus(
	ctx context.Context,
	client *kihclientset.Clientset,
	ipam *ipam.IPAllocator,
	event string,
	vmnetcfgNamespace string,
	vmnetcfgVMName string,
	ip string,
	networkName string,
	hwAddr string,
	poolName string,
) (err error) {
	// an unknown event must never reach the persisted status - falling
	// through would rebuild the allocation map from scratch and erase
	// every live allocation entry - and it is rejected before any API
	// call, so the ledger and the request counters stay untouched
	switch event {
	case EventAdd, EventDelete:
	default:
		return fmt.Errorf("unsupported ippool status event %s for ip %s in pool %s", event, ip, poolName)
	}

	// Allocation references carry the canonical MAC spelling so add and
	// delete computations agree on owner identity across retries.
	ownerRef := util.AllocationRef(vmnetcfgNamespace, vmnetcfgVMName, hwAddr)
	return updatePoolStatus(ctx, client, networkName, poolName, func(currentPool *kihv1.IPPool) (bool, error) {
		if event == EventAdd {
			if existing, exists := currentPool.Status.IPv4.Allocated[ip]; exists {
				if existing != ownerRef {
					return false, fmt.Errorf("ip %s already found in IPPool status: %w", ip, util.ErrForeignOwner)
				}
				return false, nil
			}
		}
		updatedAllocated := make(map[string]string)

		switch event {
		case EventAdd:
			for k, v := range currentPool.Status.IPv4.Allocated {
				updatedAllocated[k] = v
			}
			updatedAllocated[ip] = ownerRef
		case EventDelete:
			for k, v := range currentPool.Status.IPv4.Allocated {
				if k != ip {
					updatedAllocated[k] = v
				}
			}

			if existing, exists := currentPool.Status.IPv4.Allocated[ip]; exists && existing != ownerRef {
				return false, fmt.Errorf("allocation for ip %s belongs to %s, not removing it from the %s status: %w", ip, existing, poolName, util.ErrForeignOwner)
			}
		}
		used, available, exists := ipam.UsageCounts(networkName)
		if !exists {
			return false, fmt.Errorf("cannot update status of IPPool %s: network %q is not registered in the local allocator", poolName, networkName)
		}
		currentPool.Status.IPv4.Allocated = updatedAllocated
		currentPool.Status.IPv4.Used = used
		currentPool.Status.IPv4.Available = available
		return true, nil
	})
}

// UpdateAccounting refreshes durable usage after local claims have been
// released, without inventing a ledger mutation. A missing local subnet
// cannot establish usage and must not be published as an empty pool.
func UpdateAccounting(ctx context.Context, client *kihclientset.Clientset, ipam *ipam.IPAllocator, networkName, poolName string) error {
	return updatePoolStatus(ctx, client, networkName, poolName, func(currentPool *kihv1.IPPool) (bool, error) {
		used, available, exists := ipam.UsageCounts(networkName)
		if !exists {
			return false, fmt.Errorf("cannot update accounting of IPPool %s: network %q is not registered in the local allocator", poolName, networkName)
		}
		if currentPool.Status.IPv4.Used == used && currentPool.Status.IPv4.Available == available {
			return false, nil
		}
		currentPool.Status.IPv4.Used = used
		currentPool.Status.IPv4.Available = available
		return true, nil
	})
}

// updatePoolStatus rebases only the intended status mutation on each fresh
// read. The callback reports whether a write is required; it must not perform
// allocation or release side effects, since conflicts invoke it again.
func updatePoolStatus(ctx context.Context, client *kihclientset.Clientset, networkName, poolName string, update func(*kihv1.IPPool) (bool, error)) error {
	for retry := 0; retry < maxRetries; retry++ {
		currentPool, err := client.KubevirtiphelperV1().IPPools().Get(ctx, poolName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("cannot get IPPool %s: %w", poolName, err)
		}
		if currentPool.Spec.NetworkName != networkName {
			return fmt.Errorf("cannot update status of IPPool %s: network %q does not match expected network %q", poolName, currentPool.Spec.NetworkName, networkName)
		}

		changed, err := update(currentPool)
		if err != nil || !changed {
			return err
		}
		currentPool.Status.LastUpdate = metav1.Now()
		if _, err := client.KubevirtiphelperV1().IPPools().UpdateStatus(ctx, currentPool, metav1.UpdateOptions{}); err == nil {
			return nil
		} else {
			if apierrors.IsConflict(err) || strings.Contains(err.Error(), "please apply your changes to the latest version and try again") {
				if retry == maxRetries-1 {
					return fmt.Errorf("cannot update status of IPPool %s after %d retries: %w", poolName, maxRetries, err)
				}
			} else {
				return fmt.Errorf("cannot update status of IPPool %s: %w", poolName, err)
			}

			log.Warnf("(ippoolstatus.updatePoolStatus) cannot update status of IPPool %s after %d attempt(s), retrying in a bit",
				poolName, retry+1)

			select {
			case <-ctx.Done():
				return fmt.Errorf("cannot update status of IPPool %s: %w", poolName, ctx.Err())
			case <-time.After(time.Duration(retry) * retryDelay):
			}
		}
	}

	return fmt.Errorf("cannot update status of IPPool %s after %d retries", poolName, maxRetries)
}
