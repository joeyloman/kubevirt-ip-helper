package ippool

// R05 regression tests: the ledger revalidation resolves the owner of a
// persisted record by GET-ing the recorded vm name AS the object name.
// the controller-created binding is named after its vm, but a manually
// created (or renamed) binding can carry any object name while its spec
// attributes the nic to the vm: the name-addressed GET then either
// returns NotFound - and the verdict falls through to the vm liveness -
// or, worse, returns ANOTHER binding which merely happens to carry the
// vm's name: a same-named decoy which records none of the claim made the
// revalidation conclude ownerGone although neither the claiming binding
// nor the vm was gone, and the dropped record freed the recorded address
// for a fresh allocation while the live vm's controller was about to
// recreate its binding around it.
//
// The fix resolves the owner by the full owner tuple among the actual
// bindings whenever the name-addressed object does not confirm the
// claim: a binding whose spec attributes the recorded nic (the vm name,
// the canonical mac, the network and the address) to the recorded vm
// keeps the record, whatever its object name is called. the verdict of a
// binding which belongs to the recorded vm and no longer records the
// claim stays the authoritative positive removal, the vm-liveness
// fallback and the conservative ownerUnverified behavior are unchanged,
// and no verdict frees an address a live claim still records.

import (
	"errors"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

const (
	customOwnerNamespace = "default"
	customOwnerVMName    = "vm-x"
	customOwnerMAC       = "02:00:00:00:00:10"
	customOwnerIP        = "10.0.0.2"
)

func customOwnerRef() string {
	return util.AllocationRef(customOwnerNamespace, customOwnerVMName, customOwnerMAC)
}

// customOwnerNewStoredPool seeds the one-address pool with the ledger
// record of the scenario: the range holds exactly the recorded address,
// so a dropped record is observable as a fresh allocation receiving it.
func customOwnerNewStoredPool() *kihv1.IPPool {
	stored := recoveryNewPool("pool1", "net-a")
	stored.Status.IPv4.Allocated = map[string]string{
		customOwnerIP: customOwnerRef(),
	}

	return stored
}

// customOwnerNewClaimant builds the binding which actually claims the
// recorded address: its object name differs from the vm its spec
// attributes the nic to, like a manually created binding.
func customOwnerNewClaimant() *kihv1.VirtualMachineNetworkConfig {
	claimant := recoveryNewVMNetCfg(customOwnerNamespace, "custom-binding", customOwnerIP, customOwnerMAC, "net-a")
	claimant.Spec.VMName = customOwnerVMName
	claimant.UID = "uid-claimant"

	return claimant
}

// customOwnerNewDecoy builds a binding which is literally named after
// the recorded vm but belongs to another vm and records none of its
// claim.
func customOwnerNewDecoy() *kihv1.VirtualMachineNetworkConfig {
	decoy := recoveryNewVMNetCfg(customOwnerNamespace, customOwnerVMName, "10.9.9.9", "02:00:00:00:00:99", "net-b")
	decoy.Spec.VMName = "vm-y"
	decoy.UID = "uid-decoy"

	return decoy
}

// customOwnerNewMoved builds the binding of the recorded vm (name and
// spec both) which no longer records the claim: the positive removal the
// ownerGone verdict of the name-addressed object must keep answering.
func customOwnerNewMoved() *kihv1.VirtualMachineNetworkConfig {
	moved := recoveryNewVMNetCfg(customOwnerNamespace, customOwnerVMName, "10.9.9.9", customOwnerMAC, "net-b")
	moved.UID = "uid-moved"

	return moved
}

// TestVerifyLedgerOwnerResolvesTheRecordedOwnerAmongTheActualBindings
// pins the verdict matrix of the review scenario: a ledger record which
// references vm-x while the actual claiming binding is named
// custom-binding, crossed with the liveness of the binding, the vm and a
// second binding literally named after the vm.
func TestVerifyLedgerOwnerResolvesTheRecordedOwnerAmongTheActualBindings(t *testing.T) {
	pool := recoveryNewPool("pool1", "net-a")

	cases := []struct {
		name string
		// claimant seeds the custom-named claiming binding; nil deletes it
		claimant *kihv1.VirtualMachineNetworkConfig
		// decoy seeds a second binding literally named after the vm
		decoy *kihv1.VirtualMachineNetworkConfig
		// vmLive is the liveness of the vm the record references
		vmLive bool
		// vmQueried is whether the verdict must consult the vm at all
		vmQueried bool
		// failList switches the binding list into its failure mode
		failList bool
		want     ownerLiveness
		// wantOwner is the object name of the resolved claiming binding,
		// empty when no binding resolves the claim
		wantOwner string
	}{
		{
			name:      "live custom-named claimant, live vm",
			claimant:  customOwnerNewClaimant(),
			vmLive:    true,
			want:      ownerLive,
			wantOwner: "custom-binding",
		},
		{
			name:      "live custom-named claimant, gone vm",
			claimant:  customOwnerNewClaimant(),
			vmLive:    false,
			want:      ownerLive,
			wantOwner: "custom-binding",
		},
		{
			name:      "live custom-named claimant behind a same-named decoy, gone vm",
			claimant:  customOwnerNewClaimant(),
			decoy:     customOwnerNewDecoy(),
			vmLive:    false,
			want:      ownerLive,
			wantOwner: "custom-binding",
		},
		{
			name:      "deleted claimant behind a same-named decoy, live vm",
			decoy:     customOwnerNewDecoy(),
			vmLive:    true,
			vmQueried: true,
			want:      ownerLive,
		},
		{
			name:      "deleted claimant behind a same-named decoy, gone vm",
			decoy:     customOwnerNewDecoy(),
			vmLive:    false,
			vmQueried: true,
			want:      ownerGone,
		},
		{
			name:      "deleted claimant without a decoy, live vm",
			vmLive:    true,
			vmQueried: true,
			want:      ownerLive,
		},
		{
			name:      "deleted claimant without a decoy, gone vm",
			vmLive:    false,
			vmQueried: true,
			want:      ownerGone,
		},
		{
			name:  "the vm's own name-matching binding positively removed the claim",
			decoy: customOwnerNewMoved(),
			want:  ownerGone,
		},
		{
			name:     "unresolvable binding list keeps the record",
			claimant: customOwnerNewClaimant(),
			failList: true,
			want:     ownerUnverified,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rs, _ := recoveryNewController(t, customOwnerNewStoredPool())

			if tc.claimant != nil {
				rs.vmnetcfgs = append(rs.vmnetcfgs, tc.claimant)
			}
			if tc.decoy != nil {
				rs.vmnetcfgs = append(rs.vmnetcfgs, tc.decoy)
			}
			rs.failVMNetCfgList = tc.failList

			vmQueried := false
			c.verifyVM = func(namespace string, name string) (bool, error) {
				vmQueried = true
				if namespace != customOwnerNamespace || name != customOwnerVMName {
					t.Errorf("the vm verification queried %s/%s, want %s/%s", namespace, name, customOwnerNamespace, customOwnerVMName)
				}

				return tc.vmLive, nil
			}

			liveness, owner := c.verifyLedgerOwner(pool, customOwnerNamespace, customOwnerVMName, customOwnerMAC, customOwnerIP)

			if liveness != tc.want {
				t.Errorf("verdict = %d, want %d", liveness, tc.want)
			}
			if vmQueried != tc.vmQueried {
				t.Errorf("the vm liveness was consulted = %t, want %t", vmQueried, tc.vmQueried)
			}
			if tc.wantOwner == "" {
				if owner != nil {
					t.Errorf("resolved claiming binding = %s/%s, want none (the verdict must not attribute the record to a binding which does not claim it)", owner.Namespace, owner.Name)
				}

				return
			}
			if owner == nil {
				t.Fatalf("the claiming binding %s must resolve the record, got none", tc.wantOwner)
			}
			if owner.Name != tc.wantOwner {
				t.Errorf("resolved claiming binding = %s, want %s", owner.Name, tc.wantOwner)
			}
			if owner.Spec.VMName != customOwnerVMName {
				t.Errorf("the resolved binding attributes the nic to vm %q, want %q", owner.Spec.VMName, customOwnerVMName)
			}
		})
	}
}

// TestRegistrationKeepsTheLedgerRecordOfALiveVMBehindASameNamedDecoyBinding
// is the reproduced defect of the matrix: the claiming binding is gone,
// a binding literally named after the recorded vm belongs to another vm,
// and the vm itself is live - its controller recreates the binding and
// the recreated binding reclaims the recorded address. the name-addressed
// GET returned the decoy and the missing claim read as a positive
// removal, so the record was dropped and the recorded address was handed
// to a fresh allocation while the live vm's guest could still hold it.
func TestRegistrationKeepsTheLedgerRecordOfALiveVMBehindASameNamedDecoyBinding(t *testing.T) {
	c, rs, _ := recoveryNewController(t, customOwnerNewStoredPool())
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{customOwnerNewDecoy()}
	c.verifyVM = func(namespace string, name string) (bool, error) {
		return true, nil
	}

	if err := recoveryRegistrationSteps(t, c, recoveryNewPool("pool1", "net-a")); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	// the record of the live vm keeps its protection: it is republished
	// and pinned under the recorded identity, so the recreated binding
	// reclaims its own address
	if ref, republished := rs.lastBody.Status.IPv4.Allocated[customOwnerIP]; !republished || ref != customOwnerRef() {
		t.Errorf("republished ledger entry = %q (found %t), want the canonical record %q of the live vm", ref, republished, customOwnerRef())
	}
	if used := c.ipam.Used("net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the record of the live vm keeps its pin)", used)
	}
	if ip, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Errorf("the recorded address of the live vm must stay unavailable to a fresh allocation, got %q", ip)
	}
	if _, err := c.ipam.ReclaimIP("net-a", customOwnerIP, customOwnerRef()); err != nil {
		t.Errorf("the recreated binding of the live vm must reclaim its recorded address: %s", err)
	}
}

// TestRegistrationResolvesTheCustomNamedClaimantAmongTheActualBindings:
// the claiming binding is live under its custom object name while its vm
// is gone and a same-named decoy shadows the name-addressed lookup. the
// live claim keeps the record - the durable ownership must not depend on
// the vm outliving its own manually created binding - and the resolved
// owner is exclusive: the decoy's vm never reclaims the address.
func TestRegistrationResolvesTheCustomNamedClaimantAmongTheActualBindings(t *testing.T) {
	c, rs, _ := recoveryNewController(t, customOwnerNewStoredPool())
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{
		customOwnerNewClaimant(),
		customOwnerNewDecoy(),
	}
	c.verifyVM = func(namespace string, name string) (bool, error) {
		return false, nil
	}

	if err := recoveryRegistrationSteps(t, c, recoveryNewPool("pool1", "net-a")); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if ref, republished := rs.lastBody.Status.IPv4.Allocated[customOwnerIP]; !republished || ref != customOwnerRef() {
		t.Errorf("republished ledger entry = %q (found %t), want the canonical record %q of the live claimant", ref, republished, customOwnerRef())
	}
	if used := c.ipam.Used("net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the live claim keeps its pin)", used)
	}
	if ip, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Errorf("the recorded address must stay unavailable to a fresh allocation, got %q", ip)
	}
	if _, err := c.ipam.ReclaimIP("net-a", customOwnerIP, customOwnerRef()); err != nil {
		t.Errorf("the custom-named claimant must reclaim its recorded address through the identity of its spec.vmname: %s", err)
	}
	decoyRef := util.AllocationRef(customOwnerNamespace, "vm-y", customOwnerMAC)
	if _, err := c.ipam.ReclaimIP("net-a", customOwnerIP, decoyRef); !errors.Is(err, ipam.ErrIPForeignOwner) {
		t.Errorf("the vm of the decoy binding must stay rejected as a foreign owner of the recorded address, got err %v", err)
	}
}

// TestRegistrationKeepsTheRecordOfALiveCustomNamedClaimantWhoseVMIsGone:
// the pinning fixture of the conservative keep - the claiming binding is
// live under its custom object name, the vm is gone and no decoy exists.
// the live claim keeps its record either way: resolving it among the
// actual bindings or through the vm liveness must never free it.
func TestRegistrationKeepsTheRecordOfALiveCustomNamedClaimantWhoseVMIsGone(t *testing.T) {
	c, rs, _ := recoveryNewController(t, customOwnerNewStoredPool())
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{customOwnerNewClaimant()}
	c.verifyVM = func(namespace string, name string) (bool, error) {
		return false, nil
	}

	if err := recoveryRegistrationSteps(t, c, recoveryNewPool("pool1", "net-a")); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if ref, republished := rs.lastBody.Status.IPv4.Allocated[customOwnerIP]; !republished || ref != customOwnerRef() {
		t.Errorf("republished ledger entry = %q (found %t), want the canonical record %q of the live claimant", ref, republished, customOwnerRef())
	}
	if used := c.ipam.Used("net-a"); used != 1 {
		t.Errorf("ipam used = %d, want 1 (the live claim keeps its pin)", used)
	}
	if ip, err := c.ipam.GetIP("net-a", ""); err == nil {
		t.Errorf("the recorded address must stay unavailable to a fresh allocation, got %q", ip)
	}
}

// TestRegistrationDropsTheRecordOfADeadClaimantBehindASameNamedDecoyBinding:
// the safe drop of the matrix - the claiming binding is gone, the vm is
// gone as well and only the same-named decoy of another vm remains. the
// record is genuinely dead, so it must not be kept forever: the address
// returns to the fresh allocations.
func TestRegistrationDropsTheRecordOfADeadClaimantBehindASameNamedDecoyBinding(t *testing.T) {
	c, rs, _ := recoveryNewController(t, customOwnerNewStoredPool())
	rs.vmnetcfgs = []*kihv1.VirtualMachineNetworkConfig{customOwnerNewDecoy()}
	c.verifyVM = func(namespace string, name string) (bool, error) {
		return false, nil
	}

	if err := recoveryRegistrationSteps(t, c, recoveryNewPool("pool1", "net-a")); err != nil {
		t.Fatalf("the registration steps: %s", err)
	}

	if _, republished := rs.lastBody.Status.IPv4.Allocated[customOwnerIP]; republished {
		t.Error("the record of the dead claimant must not be republished")
	}
	if used := c.ipam.Used("net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the dead claim pins nothing)", used)
	}
	if ip, err := c.ipam.GetIP("net-a", ""); err != nil || ip != customOwnerIP {
		t.Errorf("the address of the dead claim must return to the fresh allocations, got ip %q err %v", ip, err)
	}
}
