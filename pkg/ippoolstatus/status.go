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

	for retry := 0; retry < maxRetries; retry++ {
		currentPool, err := client.KubevirtiphelperV1().IPPools().Get(ctx, poolName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("cannot get IPPool %s: %w", poolName, err)
		}

		updatedPool := currentPool.DeepCopy()
		updatedAllocated := make(map[string]string)

		// allocation references carry the canonical mac address spelling so
		// add and delete computations agree on the owner identity
		ownerRef := util.AllocationRef(vmnetcfgNamespace, vmnetcfgVMName, hwAddr)

		switch event {
		case EventAdd:
			for k, v := range currentPool.Status.IPv4.Allocated {
				if k == ip {
					if v == ownerRef {
						// the allocation reference is already recorded, so a
						// retry after a partially applied update treats it as
						// done
						return nil
					}

					return fmt.Errorf("ip %s already found in IPPool status: %w", ip, util.ErrForeignOwner)
				}
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
				return fmt.Errorf("allocation for ip %s belongs to %s, not removing it from the %s status: %w", ip, existing, poolName, util.ErrForeignOwner)
			}
		default:
			// any unknown event must never reach the persisted status:
			// falling through would rebuild the allocation map from scratch
			// and erase every live allocation entry
			return fmt.Errorf("unsupported ippool status event %s for ip %s in pool %s", event, ip, poolName)
		}
		updatedPool.Status.IPv4.Allocated = updatedAllocated
		updatedPool.Status.IPv4.Used = ipam.Used(networkName)
		updatedPool.Status.IPv4.Available = ipam.Available(networkName)
		updatedPool.Status.LastUpdate = metav1.Now()

		if _, err := client.KubevirtiphelperV1().IPPools().UpdateStatus(ctx, updatedPool, metav1.UpdateOptions{}); err == nil {
			// return success
			return nil
		} else {
			// If it's a conflict error try again
			if apierrors.IsConflict(err) || strings.Contains(err.Error(), "please apply your changes to the latest version and try again") {
				if retry == maxRetries-1 {
					return fmt.Errorf("cannot update status of IPPool %s after %d retries: %s", updatedPool.Name, maxRetries, err.Error())
				}
			} else {
				return fmt.Errorf("cannot update status of IPPool %s: %s", updatedPool.Name, err.Error())
			}

			// Wait before retrying
			log.Warnf("(ippoolstatus.UpdateStatus) [%s/%s] cannot update status of IPPool %s after %d attempt(s), retrying in a bit",
				vmnetcfgNamespace, vmnetcfgVMName, updatedPool.Name, retry+1)

			select {
			case <-ctx.Done():
				return fmt.Errorf("cannot update status of IPPool %s: %w", updatedPool.Name, ctx.Err())
			case <-time.After(time.Duration(retry) * retryDelay):
			}
		}
	}

	return fmt.Errorf("cannot update status of IPPool %s after %d retries", poolName, maxRetries)
}
