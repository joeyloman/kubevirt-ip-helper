package util

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

// ErrForeignOwner reports an IPPool status entry whose allocation
// reference belongs to another owner, so a controller must not remove it.
// callers classify the updateIPPoolStatus rejection with errors.Is.
var ErrForeignOwner = errors.New("allocation belongs to another owner")

// AllocationRef builds the canonical allocation reference of an IPAM
// reservation: the persisted IPPool status entries, the ipam owner tokens
// and the reclaim decisions all agree on this spelling, so a restoring
// binding producing it again stays idempotent. the mac address is stored
// in the canonical colon form.
func AllocationRef(namespace string, vmName string, hwAddr string) string {
	return fmt.Sprintf("%s/%s [%s]", namespace, vmName, CanonicalHWAddr(hwAddr))
}

// ParseAllocationRef splits an allocation reference built by AllocationRef
// back into its components. references which do not follow the canonical
// spelling (older revision or hand-edited status) are reported as
// unparseable (ok=false) and must be treated as unprotectable claims.
func ParseAllocationRef(ref string) (namespace string, vmName string, hwAddr string, ok bool) {
	const ownerSeparator = " ["

	ownerSep := strings.LastIndex(ref, ownerSeparator)
	if ownerSep < 0 || !strings.HasSuffix(ref, "]") {
		return "", "", "", false
	}

	mac := ref[ownerSep+len(ownerSeparator) : len(ref)-1]
	owner := ref[:ownerSep]

	slashSep := strings.Index(owner, "/")
	if slashSep < 0 {
		return "", "", "", false
	}

	namespace = owner[:slashSep]
	vmName = owner[slashSep+1:]

	if namespace == "" || vmName == "" || mac == "" || strings.ContainsAny(mac, "[]/") {
		return "", "", "", false
	}

	// only a valid mac address may act as the owner identity, so garbage
	// reference tails from hand-edited status are unprotectable
	if _, err := net.ParseMAC(mac); err != nil {
		return "", "", "", false
	}

	return namespace, vmName, mac, true
}

// IsAlreadyReleased reports ipam outcomes which state that nothing about
// the given address is left to release: a subnet name without allocation
// state, an address without a live allocation or an address which is
// outside the registered subnet at all. a plain empty ip is deliberately
// excluded: that is a caller error and must surface.
func IsAlreadyReleased(err error) bool {
	return errors.Is(err, ipam.ErrSubnetNotFound) ||
		errors.Is(err, ipam.ErrIPAlreadyFree) ||
		errors.Is(err, ipam.ErrIPNotInCidr)
}
