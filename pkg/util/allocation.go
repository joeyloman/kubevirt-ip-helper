package util

import (
	"errors"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

// ErrForeignOwner reports an IPPool status entry whose allocation
// reference belongs to another owner, so a controller must not remove
// it. callers classify the updateIPPoolStatus rejection with errors.Is.
var ErrForeignOwner = errors.New("allocation belongs to another owner")

// IsAlreadyReleased reports ipam outcomes which state that nothing about
// the given address is left to release: a subnet name without allocation
// state or an address without a live allocation. a plain empty ip is
// deliberately excluded: that is a caller error and must surface.
func IsAlreadyReleased(err error) bool {
	return errors.Is(err, ipam.ErrSubnetNotFound) ||
		errors.Is(err, ipam.ErrIPAlreadyFree)
}
