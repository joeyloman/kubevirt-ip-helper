package cache

import (
	"strings"
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	log "github.com/sirupsen/logrus"
)

func newTestPool(networkName string) *kihv1.IPPool {
	return &kihv1.IPPool{
		Spec: kihv1.IPPoolSpec{
			NetworkName: networkName,
			IPv4Config: kihv1.IPv4Config{
				ServerIP: "10.0.0.1",
				Subnet:   "10.0.0.0/24",
			},
		},
	}
}

func TestCachePoolLifecycle(t *testing.T) {
	c := New()
	pool := newTestPool("default/net-a")

	if c.Check(pool) {
		t.Fatal("Check returned true for an empty cache")
	}

	if err := c.Add(pool); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	if !c.Check(pool) {
		t.Fatal("Check returned false after Add")
	}

	got, err := c.Get("pool", "default/net-a")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	gotPool, ok := got.(kihv1.IPPool)
	if !ok {
		t.Fatalf("Get returned %T, want kihv1.IPPool", got)
	}
	if gotPool.Spec.NetworkName != "default/net-a" {
		t.Errorf("got network name %q, want default/net-a", gotPool.Spec.NetworkName)
	}
	if gotPool.Spec.IPv4Config.Subnet != "10.0.0.0/24" {
		t.Errorf("got subnet %q, want 10.0.0.0/24", gotPool.Spec.IPv4Config.Subnet)
	}

	if err := c.Delete("pool", "default/net-a"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if c.Check(pool) {
		t.Fatal("Check returned true after Delete")
	}

	if _, err := c.Get("pool", "default/net-a"); err == nil {
		t.Fatal("Get returned nil error after Delete")
	} else if want := "IPPool default/net-a does not exists in cache"; err.Error() != want {
		t.Errorf("got error %q, want %q", err, want)
	}

	if err := c.Delete("pool", "default/net-a"); err == nil {
		t.Fatal("Delete returned nil error for a missing pool")
	} else if want := "IPPool default/net-a does not exists in cache"; err.Error() != want {
		t.Errorf("got error %q, want %q", err, want)
	}
}

func TestCacheAddDuplicateCollision(t *testing.T) {
	c := New()
	if err := c.Add(newTestPool("default/net-a")); err != nil {
		t.Fatalf("first Add returned error: %v", err)
	}

	err := c.Add(newTestPool("default/net-a"))
	if err == nil {
		t.Fatal("second Add for the same network name returned nil error")
	}
	if want := "IPPool default/net-a already exists in cache"; err.Error() != want {
		t.Errorf("got error %q, want %q", err, want)
	}
}

func TestCacheCollisionKeepsFirstPool(t *testing.T) {
	c := New()
	if err := c.Add(newTestPool("default/net-a")); err != nil {
		t.Fatalf("first Add returned error: %v", err)
	}

	second := newTestPool("default/net-a")
	second.Spec.IPv4Config.Subnet = "172.16.0.0/16"
	if err := c.Add(second); err == nil {
		t.Fatal("colliding Add returned nil error")
	}

	got, err := c.Get("pool", "default/net-a")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if gotPool := got.(kihv1.IPPool); gotPool.Spec.IPv4Config.Subnet != "10.0.0.0/24" {
		t.Errorf("collision replaced the original pool: got subnet %q, want 10.0.0.0/24", gotPool.Spec.IPv4Config.Subnet)
	}
}

func TestCacheNonPoolTypesIgnored(t *testing.T) {
	c := New()

	if err := c.Add("not-a-pool"); err != nil {
		t.Fatalf("Add with a non-pool type returned error: %v", err)
	}
	if c.Check("not-a-pool") {
		t.Fatal("Check returned true for a non-pool type")
	}

	got, err := c.Get("unknown-kind", "anything")
	if err != nil {
		t.Fatalf("Get with an unknown kind returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("Get with an unknown kind returned %v, want nil", got)
	}

	if err := c.Delete("unknown-kind", "anything"); err != nil {
		t.Fatalf("Delete with an unknown kind returned error: %v", err)
	}
}

func TestCacheGetReturnsValueCopy(t *testing.T) {
	c := New()
	if err := c.Add(newTestPool("default/net-a")); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	got, err := c.Get("pool", "default/net-a")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	gotPool := got.(kihv1.IPPool)
	gotPool.Spec.IPv4Config.ServerIP = "192.0.2.99"

	again, err := c.Get("pool", "default/net-a")
	if err != nil {
		t.Fatalf("second Get returned error: %v", err)
	}
	if againPool := again.(kihv1.IPPool); againPool.Spec.IPv4Config.ServerIP != "10.0.0.1" {
		t.Errorf("cache was mutated through the Get copy: got serverip %q, want 10.0.0.1", againPool.Spec.IPv4Config.ServerIP)
	}
}

type captureHook struct {
	entries []*log.Entry
}

func (h *captureHook) Levels() []log.Level {
	return log.AllLevels
}

func (h *captureHook) Fire(e *log.Entry) error {
	h.entries = append(h.entries, e)
	return nil
}

func TestCacheUsageLogsPoolDetails(t *testing.T) {
	c := New()
	if err := c.Add(newTestPool("default/net-a")); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	hook := &captureHook{}
	old := log.StandardLogger().ReplaceHooks(log.LevelHooks{})
	defer log.StandardLogger().ReplaceHooks(old)
	log.StandardLogger().AddHook(hook)

	c.Usage("pool")
	c.Usage("unknown-kind")

	if len(hook.entries) == 0 {
		t.Fatal("Usage produced no log entries")
	}
	for _, e := range hook.entries {
		if strings.Contains(e.Message, "ipPoolCache") &&
			strings.Contains(e.Message, "default/net-a") &&
			strings.Contains(e.Message, "10.0.0.0/24") {
			return
		}
	}
	t.Errorf("Usage log entries did not mention the cached pool: %+v", hook.entries)
}
