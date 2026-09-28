package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/types"
)

// a request-less AdmissionReview (or one whose old object is missing or
// corrupt) previously made validateIPPoolAdmission dereference a nil
// request or half-write a 500 and fall through, so the apiserver read a
// garbage body as a webhook failure: with the entry's implicit
// failurePolicy Fail every IPPool deletion failed. the handler must
// answer each case with exactly one well-formed allow.
func TestValidateIPPoolAdmissionMalformedReviews(t *testing.T) {
	tests := []struct {
		name string
		body string
		uid  string
	}{
		{
			name: "request-less AdmissionReview",
			body: `{}`,
			uid:  "",
		},
		{
			name: "missing old object",
			body: `{"request":{"uid":"test-uid-1"}}`,
			uid:  "test-uid-1",
		},
		{
			name: "corrupt old object",
			body: `{"request":{"uid":"test-uid-2","oldObject":{"raw":"bm90LWFwb29s"}}}`,
			uid:  "test-uid-2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := &Handler{}
			req := httptest.NewRequest(http.MethodPost, "/validate-ippool", bytes.NewBufferString(test.body))
			rec := httptest.NewRecorder()

			h.validateIPPoolAdmission(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", rec.Code)
			}

			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("expected application/json content type, got %q", ct)
			}

			ar := &admissionv1.AdmissionReview{}
			if err := json.NewDecoder(rec.Body).Decode(ar); err != nil {
				t.Fatalf("response body is not a single AdmissionReview: %v", err)
			}

			if ar.Response == nil {
				t.Fatal("response carries no AdmissionResponse")
			}

			if !ar.Response.Allowed {
				t.Fatalf("expected the request to be allowed, denied with: %s", ar.Response.Result)
			}

			if ar.Response.UID != types.UID(test.uid) {
				t.Fatalf("response uid = %q, want the request uid %q", ar.Response.UID, test.uid)
			}
		})
	}
}
