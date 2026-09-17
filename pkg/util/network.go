package util

import (
	"fmt"
	"strings"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	NetworkLabel          = "kubevirtiphelper/network"
	NetworkNamespaceLabel = "kubevirtiphelper/network-namespace"
)

// NetworkScope is the immutable identity of one helper process. Its zero value
// owns nothing; it must never be used as an unfiltered discovery selector.
type NetworkScope struct {
	namespace   string
	name        string
	networkName string
	selector    string
	leaseName   string
}

func NewNetworkScope(namespace, name string) (NetworkScope, error) {
	if !validNetworkLabel(namespace) || !validNetworkLabel(name) {
		return NetworkScope{}, fmt.Errorf("network namespace and name must be nonempty DNS labels of at most 63 characters: %q/%q", namespace, name)
	}
	return NetworkScope{
		namespace:   namespace,
		name:        name,
		networkName: namespace + "/" + name,
		selector:    NetworkLabel + "=" + name + "," + NetworkNamespaceLabel + "=" + namespace,
		leaseName:   "kubevirt-ip-helper-lock-" + name,
	}, nil
}

func validNetworkLabel(value string) bool {
	return len(validation.IsDNS1123Label(value)) == 0
}

func (s NetworkScope) Name() string        { return s.name }
func (s NetworkScope) Namespace() string   { return s.namespace }
func (s NetworkScope) NetworkName() string { return s.networkName }
func (s NetworkScope) Selector() string {
	if s.selector == "" {
		// An empty Kubernetes selector means everything, not nothing. Keep an
		// uninitialized scope fail-closed even at a discovery boundary.
		return NetworkLabel + ",!" + NetworkLabel
	}
	return s.selector
}
func (s NetworkScope) LeaseName() string { return s.leaseName }

// Owns resolves bare references in the containing object's namespace. Comparing
// against the validated, precomputed identity avoids allocating a qualified name.
func (s NetworkScope) Owns(objectNamespace, reference string) bool {
	return s.name != "" && (reference == s.networkName ||
		(objectNamespace == s.namespace && reference == s.name))
}

func (s NetworkScope) MatchesPool(pool *kihv1.IPPool) bool {
	return s.name != "" && pool != nil &&
		pool.Labels[NetworkLabel] == s.name &&
		pool.Labels[NetworkNamespaceLabel] == s.namespace &&
		pool.Spec.NetworkName == s.networkName
}

// QualifyNetworkName returns a canonical comparison key, never a repaired stored
// value. Qualified references are independent of the containing namespace.
func QualifyNetworkName(namespace, reference string) string {
	if refNamespace, name, qualified := strings.Cut(reference, "/"); qualified {
		if !validNetworkLabel(refNamespace) || !validNetworkLabel(name) {
			return ""
		}
		return reference
	}
	if !validNetworkLabel(namespace) || !validNetworkLabel(reference) {
		return ""
	}
	return namespace + "/" + reference
}

func (s NetworkScope) FilterSpec(namespace string, entries []kihv1.NetworkConfig) []kihv1.NetworkConfig {
	if len(entries) == 0 {
		return entries
	}
	var owned []kihv1.NetworkConfig
	for _, entry := range entries {
		if s.Owns(namespace, entry.NetworkName) {
			owned = append(owned, entry)
		}
	}
	return owned
}

func (s NetworkScope) FilterStatus(namespace string, entries []kihv1.NetworkConfigStatus) []kihv1.NetworkConfigStatus {
	if len(entries) == 0 {
		return entries
	}
	var owned []kihv1.NetworkConfigStatus
	for _, entry := range entries {
		if s.Owns(namespace, entry.NetworkName) {
			owned = append(owned, entry)
		}
	}
	return owned
}

// MergeSpec replaces owned rows at their first previous position (or appends
// them if none existed). Foreign rows retain their exact values and relative
// order. Foreign replacement rows are ignored rather than granting ownership.
// Neither input slice is modified.
func (s NetworkScope) MergeSpec(namespace string, current, replacement []kihv1.NetworkConfig) []kihv1.NetworkConfig {
	owned := replacement
	for _, entry := range replacement {
		if !s.Owns(namespace, entry.NetworkName) {
			owned = s.FilterSpec(namespace, replacement)
			break
		}
	}
	first := -1
	count := 0
	for i, entry := range current {
		if s.Owns(namespace, entry.NetworkName) {
			if first == -1 {
				first = i
			}
			count++
		}
	}
	if count == 0 && len(owned) == 0 {
		return current
	}
	merged := make([]kihv1.NetworkConfig, 0, len(current)-count+len(owned))
	for i, entry := range current {
		if i == first {
			merged = append(merged, owned...)
		}
		if !s.Owns(namespace, entry.NetworkName) {
			merged = append(merged, entry)
		}
	}
	if first == -1 {
		merged = append(merged, owned...)
	}
	return merged
}

// MergeStatus applies the same owned-row replacement as MergeSpec to the status
// subresource, including status-only foreign rows.
func (s NetworkScope) MergeStatus(namespace string, current, replacement []kihv1.NetworkConfigStatus) []kihv1.NetworkConfigStatus {
	owned := replacement
	for _, entry := range replacement {
		if !s.Owns(namespace, entry.NetworkName) {
			owned = s.FilterStatus(namespace, replacement)
			break
		}
	}
	first := -1
	count := 0
	for i, entry := range current {
		if s.Owns(namespace, entry.NetworkName) {
			if first == -1 {
				first = i
			}
			count++
		}
	}
	if count == 0 && len(owned) == 0 {
		return current
	}
	merged := make([]kihv1.NetworkConfigStatus, 0, len(current)-count+len(owned))
	for i, entry := range current {
		if i == first {
			merged = append(merged, owned...)
		}
		if !s.Owns(namespace, entry.NetworkName) {
			merged = append(merged, entry)
		}
	}
	if first == -1 {
		merged = append(merged, owned...)
	}
	return merged
}
