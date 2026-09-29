package ippool

// R04 regression tests: the pool GETs and the status GET/PUT of a
// registration are name-based, so a pool which is deleted and replaced
// under the same name while its registration is in flight is
// indistinguishable from the recorded object by name alone. the delete
// event UID guards protect the delayed-delete paths, but a registration
// which already holds its informer snapshot keeps running against the
// successor: the exclude pass pins the snapshot's exclude entries into
// the successor's allocator, the status rebuild writes the snapshot's
// projection into the successor's status and the cache publishes the
// snapshot's spec - an address the successor excludes stays allocatable
// to a fresh guest, and an address only the gone pool excluded stays
// stranded in the successor's ledger.
//
// The fix follows the F09 claimant-identity abort pattern: every fresh
// pool read of the registration (the claim protection, the status
// rebuild, the metrics reset) compares the uid of the read object
// against the uid of the snapshot the attempt started with, and a
// replacement aborts the attempt with a plain retriable error before the
// next mutation. the half-applied registration is torn down by the same
// registerPoolWithTeardown wrapper every failed attempt uses, and the
// successor's own event re-registers the pool with its own projection.

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
)

// replacementNewPool builds the multi-address pool of the R04 scenario:
// the range 10.0.0.2-10.0.0.6 holds enough addresses that a successor
// can keep the range but exchange the excluded address, so a leaked old
// projection and an ignored new one are both observable in the allocator
// and the republished ledger.
func replacementNewPool(uid string, exclude []string) *kihv1.IPPool {
	pool := recoveryNewPool("pool1", "net-a")
	pool.Spec.IPv4Config.Pool.Start = "10.0.0.2"
	pool.Spec.IPv4Config.Pool.End = "10.0.0.6"
	pool.Spec.IPv4Config.Pool.Exclude = exclude
	pool.UID = types.UID(uid)

	return pool
}

// TestRegistrationAbortsWhenThePoolIsReplacedBeforeTheStatusRebuild: the
// pool is replaced under the same name (a new uid, a different exclude
// entry) between the registration snapshot and the status GET/PUT - the
// interleaving lands while the claim protection takes its vmnetcfg
// snapshot. the attempt must abort before any durable mutation of the
// successor: no status write, no cache publication, no surviving pin of
// the old projection, and the retried registration of the successor
// pins and republishes exactly the successor's exclude entry.
func TestRegistrationAbortsWhenThePoolIsReplacedBeforeTheStatusRebuild(t *testing.T) {
	oldExclude := []string{"10.0.0.3"}
	newExclude := []string{"10.0.0.4"}

	stored := replacementNewPool("uid-old", oldExclude)
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)

	successor := replacementNewPool("uid-new", newExclude)

	// the replacement lands between the registration snapshot and the
	// status rebuild: the successor keeps the networkname and the range
	// but excludes a different address of it
	rs.vmnetcfgListHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.pool = successor
	}

	// the event snapshot the registration runs with is the gone pool
	snapshot := replacementNewPool("uid-old", oldExclude)

	if err := recoveryRegistrationSteps(t, c, snapshot); err == nil {
		t.Fatal("the registration must abort when the pool was replaced under the same name mid-flight")
	}

	// the abort precedes every durable mutation of the successor
	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the aborted attempt must not write the old projection into the successor's status)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}
	// the failed attempt was torn down: none of the half-applied old
	// projection stays installed or reserved
	if c.dhcp.CheckPool("net-a") {
		t.Error("the dhcp pool of the aborted registration must be torn down")
	}
	if used := c.ipam.Used("net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the exclude pin of the old projection must not survive the abort)", used)
	}

	// the successor's own event re-registers the pool: only the
	// successor's exclude entry is pinned and republished
	rs.vmnetcfgListHook = nil
	if err := recoveryRegistrationSteps(t, c, successor); err != nil {
		t.Fatalf("the retried registration of the successor: %s", err)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.4"]; got != ipam.ExcludedOwner {
		t.Errorf("allocated[10.0.0.4] = %q, want the excluded owner of the successor's exclude entry", got)
	}
	if _, republished := rs.lastBody.Status.IPv4.Allocated["10.0.0.3"]; republished {
		t.Error("the exclude entry of the replaced pool must not be written into the successor's ledger")
	}

	// the allocator serves the successor's projection: every address of
	// the range stays allocatable except the successor's exclude entry.
	// collection is order-independent, the allocator hands the free
	// addresses out in map order
	handed := map[string]bool{}
	for i := range 4 {
		ip, ipErr := c.ipam.GetIP("net-a", "")
		if ipErr != nil {
			t.Fatalf("fresh allocation %d: %s", i, ipErr)
		}
		handed[ip] = true
	}
	if handed["10.0.0.4"] {
		t.Error("the successor's excluded address must never be handed to a fresh allocation")
	}
	if !handed["10.0.0.3"] {
		t.Error("the exclude entry of the replaced pool must not strand one of the successor's addresses")
	}

	// the published cache carries the successor's projection
	published, pubErr := c.cache.Get("pool", "net-a")
	if pubErr != nil {
		t.Fatalf("the retried registration must publish the successor: %s", pubErr)
	}
	if got := published.(kihv1.IPPool).Spec.IPv4Config.Pool.Exclude; !reflect.DeepEqual(got, newExclude) {
		t.Errorf("published exclude entries = %v, want the successor's %v", got, newExclude)
	}
}

// TestRegistrationAbortsWhenThePoolIsReplacedBeforeItsClaimsAreProtected:
// the replacement lands right after the first pool read of the
// registration, so the attempt has already installed the host state and
// pinned the snapshot's exclude entry when the claim protection reads
// the successor. the abort must still precede the status write and the
// publication, the teardown must remove the half-applied registration,
// and the retried registration of the successor must converge on the
// successor's projection.
func TestRegistrationAbortsWhenThePoolIsReplacedBeforeItsClaimsAreProtected(t *testing.T) {
	oldExclude := []string{"10.0.0.3"}
	newExclude := []string{"10.0.0.4"}

	stored := replacementNewPool("uid-old", oldExclude)
	stored.Status.IPv4.Allocated = map[string]string{}

	c, rs, _ := recoveryNewController(t, stored)

	successor := replacementNewPool("uid-new", newExclude)

	// the replacement lands after the first stored-pool read of the
	// attempt and is idempotent, so the reads of the retried registration
	// of the successor never fire it again
	rs.poolGetHook = func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		if rs.pool.UID == types.UID("uid-old") {
			rs.pool = successor
		}
	}

	snapshot := replacementNewPool("uid-old", oldExclude)

	if err := recoveryRegistrationSteps(t, c, snapshot); err == nil {
		t.Fatal("the registration must abort when the pool was replaced under the same name before its claims were protected")
	}

	if rs.putCount != 0 {
		t.Errorf("pool status writes = %d, want 0 (the aborted attempt must not write the old projection into the successor's status)", rs.putCount)
	}
	if _, cacheErr := c.cache.Get("pool", "net-a"); cacheErr == nil {
		t.Error("the pool must not be published into the cache")
	}
	// the half-applied registration (the nic address, the dhcp pool, the
	// subnet and the old exclude pin) was torn down with the attempt
	if c.dhcp.CheckPool("net-a") {
		t.Error("the dhcp pool of the aborted registration must be torn down")
	}
	if used := c.ipam.Used("net-a"); used != 0 {
		t.Errorf("ipam used = %d, want 0 (the exclude pin of the old projection must not survive the abort)", used)
	}

	if err := recoveryRegistrationSteps(t, c, successor); err != nil {
		t.Fatalf("the retried registration of the successor: %s", err)
	}
	if got := rs.lastBody.Status.IPv4.Allocated["10.0.0.4"]; got != ipam.ExcludedOwner {
		t.Errorf("allocated[10.0.0.4] = %q, want the excluded owner of the successor's exclude entry", got)
	}
	if _, republished := rs.lastBody.Status.IPv4.Allocated["10.0.0.3"]; republished {
		t.Error("the exclude entry of the replaced pool must not be written into the successor's ledger")
	}
	if ip, err := c.ipam.GetIP("net-a", "10.0.0.4"); err == nil {
		t.Errorf("the successor's excluded address must stay unallocatable, got %q", ip)
	}
	if _, err := c.ipam.GetIP("net-a", "10.0.0.3"); err != nil {
		t.Errorf("the exclude entry of the replaced pool must not strand one of the successor's addresses: %s", err)
	}
}
