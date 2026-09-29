package admission

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// hungAPIClientset returns a clientset whose apiserver parks every
// request until the request's own context is done: a client call against
// it completes only when the caller's context is canceled or expires.
// the parked handlers additionally watch the fixture's release channel,
// which the cleanup closes before the server: a caller which lost its
// context wiring would hang its own bounded guard, never the cleanup.
func hungAPIClientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(func() {
		// release the parked handlers before the close: the close waits for
		// the outstanding requests, and a caller which abandoned its
		// connection on a context deadline may leave its server-side
		// handler parked on the request context alone
		close(release)
		server.Close()
	})

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building clientset: %v", err)
	}

	return clientset
}

// boundedResult waits for a call's error with a hard test deadline, so a
// call which ignores its cancellation or budget fails the test instead of
// hanging the suite.
func boundedResult(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return within the test bound")

		return nil
	}
}

// TestAddValidatingWebhookConfigurationBudgetBoundsTheHungAPI pins the
// F08 aggregate registration budget: the webhook configuration
// registration one-shots (the existence check, the ca bundle read, the
// create/update) must not outlive the budget when the apiserver stalls.
// before the fix every call used context.TODO() and a hung apiserver
// hung the boot path unbounded.
func TestAddValidatingWebhookConfigurationBudgetBoundsTheHungAPI(t *testing.T) {
	oldBudget := webhookRegistrationBudget
	webhookRegistrationBudget = 100 * time.Millisecond
	t.Cleanup(func() { webhookRegistrationBudget = oldBudget })

	h := &Handler{
		ctx:                         context.Background(),
		clientset:                   hungAPIClientset(t),
		validatingWebhookConfigName: "test-vwc",
	}

	done := make(chan error, 1)
	go func() {
		done <- h.AddValidatingWebhookConfiguration()
	}()

	if err := boundedResult(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the hung registration did not fail with the exhausted budget, got %v", err)
	}
}

// TestAddValidatingWebhookConfigurationAbortsOnCanceledContext pins the
// F08 era propagation: a canceled era must abort the registration and
// surface the cancellation instead of publishing a configuration read
// with a dead context.
func TestAddValidatingWebhookConfigurationAbortsOnCanceledContext(t *testing.T) {
	eraCtx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &Handler{
		ctx:                         eraCtx,
		clientset:                   hungAPIClientset(t),
		validatingWebhookConfigName: "test-vwc",
	}

	done := make(chan error, 1)
	go func() {
		done <- h.AddValidatingWebhookConfiguration()
	}()

	if err := boundedResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("the canceled era did not abort the registration, got %v", err)
	}
}
