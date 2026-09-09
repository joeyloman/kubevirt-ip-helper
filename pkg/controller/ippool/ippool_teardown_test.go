package ippool

import (
	"strings"
	"testing"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/network"
)

// A registration which fails midway must be torn back down on every event
// path: the leftover sub-resources would claim the networkname, so every
// retried attempt is rejected by the duplicate-networkname check and the
// network stays unregistered until the process is restarted. the ADD path
// always honored the cleanup flag, the UPDATE re-registration path
// discarded it.

// stubNicMutation replaces the netlink mutation with a no-op and restores
// the real one afterwards, so a registration attempt gets past the
// interface setup and fails at the dhcp listener of the missing
// bindinterface instead: a failure with a partially applied registration.
func stubNicMutation(t *testing.T) {
	t.Helper()

	orig := network.AddIpToNic
	network.AddIpToNic = func(nic string, ip4 string) error {
		return nil
	}
	t.Cleanup(func() {
		network.AddIpToNic = orig
	})
}

// the failed re-registration of an unregistered pool must tear its
// partial registration down, so the retry attempts the registration again
// instead of being rejected by the leftover dhcp pool of its own previous
// attempt
func TestSyncUpdateFailedRegistrationTearsDownPartialState(t *testing.T) {
	stubNicMutation(t)

	appStatus := APP_INIT
	countCurrent := 0
	indexer := newTestIndexer()
	if err := indexer.Add(testPool("pool-r2", "net-fresh2", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent)

	event := testPoolEvent("pool-r2", UPDATE, "net-fresh2")

	err := controller.sync(event)
	if err == nil {
		t.Fatal("the registration attempt returned nil, want the dhcp listener failure of the missing bindinterface")
	}
	if strings.Contains(err.Error(), "already registered by another IPPool") {
		t.Fatalf("the first attempt was rejected by the duplicate check, want the environmental failure: %v", err)
	}

	// the partial registration must not claim the networkname
	if controller.dhcp.CheckPool("net-fresh2") {
		t.Error("the partially applied dhcp pool must be torn down with the failed attempt")
	}

	// the retry reaches the registration again instead of the wedge
	err = controller.sync(event)
	if err == nil {
		t.Fatal("the retried registration returned nil, want the environmental failure again")
	}
	if strings.Contains(err.Error(), "already registered by another IPPool") {
		t.Fatalf("the retry is wedged by the leftover sub-resources of the failed attempt: %v", err)
	}

	// the transient failure keeps the pool uncounted for the startup gate:
	// the retry settles it once the environment is repaired
	if countCurrent != 0 {
		t.Errorf("ippool count = %d, want 0: the transiently failing registration must stay uncounted", countCurrent)
	}
}

// the ADD path keeps tearing down partially applied registrations after
// the shared registerPoolWithTeardown helper absorbed its inline cleanup
func TestSyncAddFailedRegistrationTearsDownPartialState(t *testing.T) {
	stubNicMutation(t)

	appStatus := APP_INIT
	countCurrent := 0
	indexer := newTestIndexer()
	if err := indexer.Add(testPool("pool-a2", "net-a2", 60)); err != nil {
		t.Fatalf("seeding indexer: %v", err)
	}

	controller, _ := newTestController(t, newTestQueue(), indexer, nil, &appStatus, &countCurrent)

	err := controller.sync(testPoolEvent("pool-a2", ADD, "net-a2"))
	if err == nil {
		t.Fatal("the registration attempt returned nil, want the dhcp listener failure of the missing bindinterface")
	}
	if strings.Contains(err.Error(), "already registered by another IPPool") {
		t.Fatalf("the attempt was rejected by the duplicate check, want the environmental failure: %v", err)
	}
	if controller.dhcp.CheckPool("net-a2") {
		t.Error("the partially applied dhcp pool must be torn down with the failed attempt")
	}
}
