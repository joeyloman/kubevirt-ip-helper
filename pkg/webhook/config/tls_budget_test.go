package config

// F08 regressions: the csr issuance must be bounded and cancelable. the
// create, approval and poll calls used to run on context.TODO() with a
// wall-clock deadline checked only between them, so one blocked call
// could outlast the 60s budget the loop believed it enforced, and a
// canceled process still burned the full poll. the whole issuance now
// runs under one deadline derived from the handler's process context,
// and the secret calls abort with that context as well.

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
		t.Fatal("the call did not return although its context was canceled or its budget exhausted")

		return nil
	}
}

// TestCreateAndSignCSRBudgetBoundsTheHungSigner: even against an
// apiserver which never answers, the issuance returns within its
// aggregate budget - a single blocked call cannot outlast the budget
// anymore.
func TestCreateAndSignCSRBudgetBoundsTheHungSigner(t *testing.T) {
	oldBudget := csrSignBudget
	csrSignBudget = 100 * time.Millisecond
	t.Cleanup(func() { csrSignBudget = oldBudget })

	h := &Handler{
		ctx:              context.Background(),
		clientset:        hungAPIClientset(t),
		csrName:          "webhook.test.svc",
		webhookNamespace: "test",
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.createAndSignCSR([]byte("test-csr"))
		done <- err
	}()

	err := boundedResult(t, done)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("createAndSignCSR against a hung apiserver = %v, want the budget's DeadlineExceeded", err)
	}
}

// TestCreateAndSignCSRAbortsOnCanceledContext: a process context which is
// already dead (the shutdown canceled it) aborts the issuance before any
// call waits.
func TestCreateAndSignCSRAbortsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &Handler{
		ctx:              ctx,
		clientset:        hungAPIClientset(t),
		csrName:          "webhook.test.svc",
		webhookNamespace: "test",
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.createAndSignCSR([]byte("test-csr"))
		done <- err
	}()

	err := boundedResult(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("createAndSignCSR on a canceled process context = %v, want the propagated context.Canceled", err)
	}
}

// TestGetSecretAbortsOnCanceledContext: the secret reads abort with the
// process context instead of hanging against a stalled apiserver.
func TestGetSecretAbortsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &Handler{
		ctx:               ctx,
		clientset:         hungAPIClientset(t),
		webhookSecretName: "webhook-tls",
		webhookNamespace:  "test",
	}

	done := make(chan error, 1)
	go func() {
		// getSecret reports absence as an empty secret, so the observable
		// is the bounded return itself
		_ = h.getSecret()
		done <- nil
	}()

	if err := boundedResult(t, done); err != nil {
		t.Errorf("getSecret = %v, want the empty-secret fallback", err)
	}
}
