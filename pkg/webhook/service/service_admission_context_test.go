package service

// F08 regressions: the admission path's list calls must run on the
// incoming request's context, so a webhook call the apiserver drops (its
// webhook timeout fired) aborts the lists instead of lingering against a
// stalled apiserver; and the server drain must be bounded, so the renewal
// restart which calls Stop synchronously never wedges behind a stalled
// connection.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
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

// TestAdmissionListsAbortWithTheRequestContext pins the F08 request
// context propagation: a vmnetcfg admission which needs both lists (the
// ippool list of the address check and the namespace list of the duplicate
// check) must return as soon as the apiserver dropped the webhook call -
// the pre-fix lists ran on context.TODO() and hung forever.
func TestAdmissionListsAbortWithTheRequestContext(t *testing.T) {
	h := &Handler{clientset: hungAPIClientset(t)}

	// a vmnetcfg with an explicit ipaddress on a network: the validation
	// needs the ippool list, the duplicate check the namespace list
	body := `{"request":{"uid":"test-uid","object":{"apiVersion":"kubevirtiphelper.k8s.binbash.org/v1","kind":"VirtualMachineNetworkConfig","metadata":{"name":"vmnc-a","namespace":"default"},"spec":{"vmname":"vm-a","networkconfig":[{"ipaddress":"10.10.10.5","macaddress":"aa:bb:cc:dd:ee:01","networkname":"net-a"}]}}}}`
	req := httptest.NewRequest(http.MethodPost, "/validate-vmnetcfg", bytes.NewBufferString(body))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.validateVmNetCfgAdmission(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the admission handler did not return although its request context was canceled")
	}

	// the aborted lists fail open: the response is the well-formed allow
	// of the webhook's fail-open contract, never a half-written hang
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(rec.Body).Decode(&ar); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if ar.Response == nil || !ar.Response.Allowed {
		t.Errorf("response = %+v, want the fail-open allow of the aborted lists", ar.Response)
	}
}

// TestStopDrainsWithinItsBudget pins the F08 drain bound: Shutdown used
// to run on the process context, which has no deadline, so a single
// stalled connection held the renewal restart which calls Stop
// synchronously forever.
func TestStopDrainsWithinItsBudget(t *testing.T) {
	oldBudget := httpDrainBudget
	httpDrainBudget = 100 * time.Millisecond
	t.Cleanup(func() { httpDrainBudget = oldBudget })

	entered := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stalled", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		// a stalled connection which ignores its own context: the worst
		// case of the drain
		<-release
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	// an in-flight request the drain must wait for
	go func() {
		client := &http.Client{}
		//nolint:bodyclose // the parked response body is released by the cleanup
		_, _ = client.Get(fmt.Sprintf("http://%s/stalled", ln.Addr().String()))
	}()
	<-entered

	h := &Handler{httpServer: srv}
	done := make(chan error, 1)
	go func() {
		done <- h.Stop()
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Stop behind a stalled connection = %v, want the drain budget's DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return although its drain budget expired")
	}
}
