package ippoolstatus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
)

// fakePoolAPI models the IPPool status endpoint of a competing writer:
// every PUT either answers with a resource-version conflict (which the
// retry must re-read and re-apply), with a forced error code, or applies
// the update to the stored pool.
type fakePoolAPI struct {
	mu                    sync.Mutex
	pool                  *kihv1.IPPool
	conflicts             int // remaining conflict responses before an apply
	replacementOnConflict *kihv1.IPPool
	getCode               int // forced GET error code (0 = return pool)
	putCode               int // forced non-conflict error code (0 = apply)
	getCount              int
	putCount              int
}

func (f *fakePoolAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		f.getCount++
		if f.getCode != 0 {
			writeStatusError(w, f.getCode, "pool lookup failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(f.pool); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case http.MethodPut:
		f.putCount++
		if f.putCode != 0 {
			writeStatusError(w, f.putCode, "boom")
			return
		}
		if f.conflicts > 0 {
			f.conflicts--
			if f.replacementOnConflict != nil {
				f.pool = f.replacementOnConflict.DeepCopy()
				f.replacementOnConflict = nil
			}
			writeStatusError(w, http.StatusConflict, "please apply your changes to the latest version and try again")
			return
		}

		var updated kihv1.IPPool
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updated.ResourceVersion = "2"
		f.pool = &updated

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&updated); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}
}

func writeStatusError(w http.ResponseWriter, code int, message string) {
	status := &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Failure",
		Message:  message,
	}
	if code == http.StatusConflict {
		status.Reason = metav1.StatusReasonConflict
	} else if code == http.StatusNotFound {
		status.Reason = metav1.StatusReasonNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func newUpdateStatusEnv(t *testing.T, conflicts int, putCode int) (context.Context, *kihclientset.Clientset, *ipam.IPAllocator, *fakePoolAPI) {
	t.Helper()

	api := &fakePoolAPI{
		pool: &kihv1.IPPool{
			TypeMeta:   metav1.TypeMeta{Kind: "IPPool", APIVersion: "kubevirtiphelper.k8s.binbash.org/v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", ResourceVersion: "1"},
			Spec:       kihv1.IPPoolSpec{NetworkName: "ns/net-a"},
		},
		conflicts: conflicts,
		putCode:   putCode,
	}
	srv := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(srv.Close)

	client, err := kihclientset.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("creating clientset: %s", err)
	}

	allocator := ipam.NewIPAllocator()
	if err := allocator.NewSubnet("ns/net-a", "10.0.0.0/24", "10.0.0.1", "10.0.0.3"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}
	return context.Background(), client, allocator, api
}

// A conflict is a competing writer which already advanced the ledger: the
// retry re-reads the pool and applies the mutation on top of the fresh
// state instead of failing the sync.
func TestUpdateStatusRetriesConflictsThenSucceeds(t *testing.T) {
	ctx, client, allocator, api := newUpdateStatusEnv(t, 2, 0)

	if err := UpdateStatus(ctx, client, allocator, EventAdd, "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 3 || api.putCount != 3 {
		t.Errorf("get/put counts = %d/%d, want 3/3 (two conflicts, then an apply)", api.getCount, api.putCount)
	}
	if got := api.pool.Status.IPv4.Allocated["10.0.0.5"]; got != "ns/vm-a [02:00:00:00:00:01]" {
		t.Errorf("ledger entry = %q, want the canonical owner reference", got)
	}
}

// An exhausted conflict budget must surface as an error, not loop forever.
func TestUpdateStatusConflictExhaustionIsAnError(t *testing.T) {
	ctx, client, allocator, api := newUpdateStatusEnv(t, 99, 0)

	err := UpdateStatus(ctx, client, allocator, EventAdd, "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a")
	if err == nil {
		t.Fatal("expected an error after the conflict budget is exhausted")
	}
	if !strings.Contains(err.Error(), "after 10 retries") {
		t.Errorf("error = %q, want the retry-exhaustion message", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.putCount != 10 {
		t.Errorf("put attempts = %d, want 10 (the retry budget)", api.putCount)
	}
}

// A non-conflict API error is not retried: the mutation either applied or
// failed with a definitive answer.
func TestUpdateStatusNonConflictErrorIsNotRetried(t *testing.T) {
	ctx, client, allocator, api := newUpdateStatusEnv(t, 0, http.StatusInternalServerError)

	err := UpdateStatus(ctx, client, allocator, EventAdd, "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a")
	if err == nil {
		t.Fatal("expected the forced server error")
	}
	if !strings.Contains(err.Error(), "cannot update status") {
		t.Errorf("error = %q, want the status-update failure message", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.putCount != 1 {
		t.Errorf("put attempts = %d, want 1 (no retry on a definitive error)", api.putCount)
	}
}

// A canceled era must abort the retry backoff instead of burning it: the
// cancellation lands strictly inside a nonzero retry sleep (the deadline
// fires while the second retry waits its 200ms), so the ctx-aware select
// of the retry loop is what returns - not client-go's ctx propagation on
// the opening request. a pre-canceled context would fail the GET client
// side and never reach the select, pinning nothing about the backoff.
func TestUpdateStatusCtxCancelDuringRetryWaitAborts(t *testing.T) {
	_, client, allocator, api := newUpdateStatusEnv(t, 99, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	// the cumulative retry waits are 0ms + 100ms + 200ms before the next
	// attempt, so the 250ms deadline deterministically fires inside the
	// 200ms sleep of the second retry, long after the live requests
	start := time.Now()
	err := UpdateStatus(ctx, client, allocator, EventAdd, "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a")
	if err == nil {
		t.Fatal("expected an error when the context expires during the retry wait")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the wrapped context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("abort after %s, want the backoff cut short by the context", elapsed)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	// the deadline must land inside a retry sleep of some attempt - which
	// one depends on scheduling, so the count is not pinned to an exact
	// phase: at least two conflicts must have been delivered and the
	// aborted run must stay well under the full retry budget
	if api.putCount < 2 || api.putCount >= maxRetries {
		t.Errorf("put attempts = %d, want >= 2 and < %d (the backoff cut short)", api.putCount, maxRetries)
	}
	if api.putCount != api.getCount {
		t.Errorf("get/put counts = %d/%d, want one read per write attempt", api.getCount, api.putCount)
	}
}

// An unknown event must never rebuild the allocation map: it is rejected
// before any API call, so the ledger and the request counters stay
// untouched.
func TestUpdateStatusUnknownEventDoesNotTouchLedger(t *testing.T) {
	_, client, allocator, api := newUpdateStatusEnv(t, 0, 0)
	api.mu.Lock()
	api.pool.Status.IPv4.Allocated = map[string]string{"10.0.0.9": "someone-else"}
	api.mu.Unlock()

	err := UpdateStatus(context.Background(), client, allocator, "bogus", "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a")
	if err == nil {
		t.Fatal("expected an error for an unknown event")
	}
	if !strings.Contains(err.Error(), "unsupported ippool status event") {
		t.Errorf("error = %q, want the unknown-event message", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 0 || api.putCount != 0 {
		t.Errorf("get/put counts = %d/%d, want 0/0 (rejected before any API call)", api.getCount, api.putCount)
	}
	if got := api.pool.Status.IPv4.Allocated["10.0.0.9"]; got != "someone-else" {
		t.Errorf("existing ledger entry = %q, want it untouched", got)
	}
}

// A pool name can resolve to a different network after a conflict. Even an
// owner-matching allocation there must not be removed by this network's cleanup.
func TestUpdateStatusConflictRetryRejectsDifferentNetwork(t *testing.T) {
	ctx, client, allocator, api := newUpdateStatusEnv(t, 1, 0)
	api.pool.Status.IPv4.Allocated = map[string]string{
		"10.0.0.5": "ns/vm-a [02:00:00:00:00:01]",
	}
	replacement := api.pool.DeepCopy()
	replacement.ResourceVersion = "2"
	replacement.Spec.NetworkName = "ns/net-b"
	replacement.Status.IPv4.Allocated["10.0.0.9"] = "ns/vm-b [02:00:00:00:00:02]"
	api.replacementOnConflict = replacement

	err := UpdateStatus(ctx, client, allocator, EventDelete, "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a")
	if err == nil || errors.Is(err, util.ErrForeignOwner) {
		t.Fatalf("network mismatch = %v, want an error that cannot acknowledge foreign-owner cleanup", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 2 || api.putCount != 1 {
		t.Errorf("get/put counts = %d/%d, want 2/1 (reject the fresh pool before retrying the write)", api.getCount, api.putCount)
	}
	if !reflect.DeepEqual(api.pool, replacement) {
		t.Errorf("replacement pool changed: got %+v, want %+v", api.pool, replacement)
	}
}

// A matching owner is not evidence of convergence when the initial lookup
// names another network; the network fence must precede the add fast path.
func TestUpdateStatusInitialNetworkMismatchCannotConverge(t *testing.T) {
	ctx, client, allocator, api := newUpdateStatusEnv(t, 0, 0)
	api.pool.Spec.NetworkName = "ns/net-b"
	api.pool.Status.IPv4.Allocated = map[string]string{
		"10.0.0.5": "ns/vm-a [02:00:00:00:00:01]",
	}
	before := api.pool.DeepCopy()

	err := UpdateStatus(ctx, client, allocator, EventAdd, "ns", "vm-a", "10.0.0.5", "ns/net-a", "02:00:00:00:00:01", "pool-a")
	if err == nil || errors.Is(err, util.ErrForeignOwner) {
		t.Fatalf("network mismatch = %v, want an error rather than a converged allocation", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 1 || api.putCount != 0 {
		t.Errorf("get/put counts = %d/%d, want 1/0 (reject the initial lookup)", api.getCount, api.putCount)
	}
	if !reflect.DeepEqual(api.pool, before) {
		t.Errorf("mismatched pool changed: got %+v, want %+v", api.pool, before)
	}
}

func newUpdateAccountingEnv(t *testing.T, conflicts int) (context.Context, *kihclientset.Clientset, *ipam.IPAllocator, *fakePoolAPI) {
	t.Helper()
	ctx, client, allocator, api := newUpdateStatusEnv(t, conflicts, 0)
	if _, err := allocator.ReclaimIP("ns/net-a", "10.0.0.1", "ns/local-vm [02:00:00:00:00:01]"); err != nil {
		t.Fatalf("ReclaimIP: %v", err)
	}
	api.pool.UID = "original-pool"
	api.pool.Labels = map[string]string{"managed-by": "another-controller"}
	api.pool.Annotations = map[string]string{"note": "preserve"}
	api.pool.Finalizers = []string{"example.org/protect"}
	api.pool.Status.LastUpdate = metav1.NewTime(time.Unix(100, 0).UTC())
	api.pool.Status.LastUpdateBeforeStart = metav1.NewTime(time.Unix(50, 0).UTC())
	// Accounting is not ledger reconciliation: even an owner disagreement
	// with the allocator and an address absent from it must survive.
	api.pool.Status.IPv4.Allocated = map[string]string{
		"10.0.0.1": "ns/foreign-vm [02:00:00:00:00:02]",
		"10.0.0.9": "ns/other-vm [02:00:00:00:00:03]",
	}
	api.pool.Status.IPv4.Used = 7
	api.pool.Status.IPv4.Available = 9
	return ctx, client, allocator, api
}

func TestUpdateAccountingPreservesFreshLedgerAndMetadata(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conflicts int
	}{
		{name: "refresh"},
		{name: "rebase after conflict", conflicts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, client, allocator, api := newUpdateAccountingEnv(t, tc.conflicts)
			want := api.pool.DeepCopy()
			if tc.conflicts != 0 {
				replacement := api.pool.DeepCopy()
				replacement.ResourceVersion = "2"
				replacement.UID = "replacement-pool"
				replacement.Labels["managed-by"] = "new-controller"
				replacement.Annotations["note"] = "concurrent change"
				replacement.Finalizers = append(replacement.Finalizers, "example.org/second")
				delete(replacement.Status.IPv4.Allocated, "10.0.0.9")
				replacement.Status.IPv4.Allocated["10.0.0.2"] = "ns/new-vm [02:00:00:00:00:04]"
				replacement.Status.LastUpdateBeforeStart = metav1.NewTime(time.Unix(75, 0).UTC())
				api.replacementOnConflict = replacement
				want = replacement.DeepCopy()
			}
			previousUpdate := want.Status.LastUpdate

			if err := UpdateAccounting(ctx, client, allocator, "ns/net-a", "pool-a"); err != nil {
				t.Fatalf("UpdateAccounting: %v", err)
			}

			api.mu.Lock()
			defer api.mu.Unlock()
			if api.getCount != tc.conflicts+1 || api.putCount != tc.conflicts+1 {
				t.Errorf("get/put counts = %d/%d, want %d/%d", api.getCount, api.putCount, tc.conflicts+1, tc.conflicts+1)
			}
			if !api.pool.Status.LastUpdate.After(previousUpdate.Time) {
				t.Errorf("LastUpdate = %v, want newer than %v", api.pool.Status.LastUpdate, previousUpdate)
			}
			want.ResourceVersion = api.pool.ResourceVersion // assigned by the API write
			want.Status.IPv4.Used = 1
			want.Status.IPv4.Available = 2
			want.Status.LastUpdate = api.pool.Status.LastUpdate
			if !apiequality.Semantic.DeepEqual(api.pool, want) {
				t.Errorf("accounting refresh changed more than counters/timestamp:\ngot  %+v\nwant %+v", api.pool, want)
			}
		})
	}
}

func TestUpdateAccountingConflictRetryRejectsDifferentNetwork(t *testing.T) {
	ctx, client, allocator, api := newUpdateAccountingEnv(t, 1)
	replacement := api.pool.DeepCopy()
	replacement.ResourceVersion = "2"
	replacement.Spec.NetworkName = "ns/net-b"
	// Matching counters cannot bypass the fresh network fence.
	replacement.Status.IPv4.Used = 1
	replacement.Status.IPv4.Available = 2
	api.replacementOnConflict = replacement

	err := UpdateAccounting(ctx, client, allocator, "ns/net-a", "pool-a")
	if err == nil || errors.Is(err, util.ErrForeignOwner) {
		t.Fatalf("network mismatch = %v, want an ordinary error rather than foreign-owner convergence", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 2 || api.putCount != 1 {
		t.Errorf("get/put counts = %d/%d, want 2/1 (reject the fresh pool before writing)", api.getCount, api.putCount)
	}
	if !reflect.DeepEqual(api.pool, replacement) {
		t.Errorf("replacement pool changed: got %+v, want %+v", api.pool, replacement)
	}
}

func TestUpdateAccountingMissingSubnetCannotConverge(t *testing.T) {
	ctx, client, _, api := newUpdateStatusEnv(t, 0, 0)
	allocator := ipam.NewIPAllocator()
	api.pool.Status.IPv4.Allocated = map[string]string{"10.0.0.1": "ns/foreign-vm"}
	api.pool.Status.LastUpdate = metav1.NewTime(time.Unix(100, 0).UTC())
	before := api.pool.DeepCopy()
	// The zero counters match UsageCounts' missing-subnet values, but do
	// not make an absent allocator network a valid accounting snapshot.
	err := UpdateAccounting(ctx, client, allocator, "ns/net-a", "pool-a")
	if err == nil || errors.Is(err, util.ErrForeignOwner) {
		t.Fatalf("missing subnet = %v, want an ordinary error rather than convergence", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.putCount != 0 {
		t.Errorf("put attempts = %d, want 0 for a missing allocator network", api.putCount)
	}
	if !reflect.DeepEqual(api.pool, before) {
		t.Errorf("pool changed without allocator state: got %+v, want %+v", api.pool, before)
	}
}

func TestUpdateAccountingUnchangedCountersSkipWrite(t *testing.T) {
	ctx, client, allocator, api := newUpdateAccountingEnv(t, 0)
	api.pool.Status.IPv4.Used = 1
	api.pool.Status.IPv4.Available = 2
	before := api.pool.DeepCopy()

	if err := UpdateAccounting(ctx, client, allocator, "ns/net-a", "pool-a"); err != nil {
		t.Fatalf("UpdateAccounting: %v", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 1 || api.putCount != 0 {
		t.Errorf("get/put counts = %d/%d, want 1/0 for unchanged accounting", api.getCount, api.putCount)
	}
	if !reflect.DeepEqual(api.pool, before) {
		t.Errorf("unchanged accounting rewrote the pool or LastUpdate: got %+v, want %+v", api.pool, before)
	}
}

func TestUpdateAccountingNotFoundRemainsRecognizable(t *testing.T) {
	ctx, client, allocator, api := newUpdateAccountingEnv(t, 0)
	api.getCode = http.StatusNotFound

	err := UpdateAccounting(ctx, client, allocator, "ns/net-a", "pool-a")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("error = %v, want a recognizable NotFound", err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if api.getCount != 1 || api.putCount != 0 {
		t.Errorf("get/put counts = %d/%d, want 1/0 for a missing pool", api.getCount, api.putCount)
	}
}

func TestUpdateStatusMissingSubnetBlocksLedgerMutation(t *testing.T) {
	for _, event := range []string{EventAdd, EventDelete} {
		t.Run(event, func(t *testing.T) {
			ctx, client, _, api := newUpdateStatusEnv(t, 0, 0)
			allocator := ipam.NewIPAllocator()
			api.pool.Status.IPv4.Allocated = map[string]string{"10.0.0.9": "ns/foreign-vm"}
			if event == EventDelete {
				api.pool.Status.IPv4.Allocated["10.0.0.1"] = "ns/vm-a [02:00:00:00:00:01]"
			}
			api.pool.Status.IPv4.Used = 7
			api.pool.Status.IPv4.Available = 9
			api.pool.Status.LastUpdate = metav1.NewTime(time.Unix(100, 0).UTC())
			before := api.pool.DeepCopy()

			err := UpdateStatus(ctx, client, allocator, event, "ns", "vm-a", "10.0.0.1", "ns/net-a", "02:00:00:00:00:01", "pool-a")
			if err == nil || errors.Is(err, util.ErrForeignOwner) {
				t.Fatalf("missing subnet = %v, want an ordinary error that cannot acknowledge cleanup", err)
			}

			api.mu.Lock()
			defer api.mu.Unlock()
			if api.putCount != 0 {
				t.Errorf("put attempts = %d, want 0 without allocator state", api.putCount)
			}
			if !reflect.DeepEqual(api.pool, before) {
				t.Errorf("ledger mutation changed pool without allocator state: got %+v, want %+v", api.pool, before)
			}
		})
	}
}
