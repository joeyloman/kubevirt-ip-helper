package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Handler struct {
	ctx        context.Context
	httpServer *http.Server
}

func Register(ctx context.Context) *Handler {
	return &Handler{
		ctx: ctx,
	}
}

func validateIPPool(ar *admissionv1.AdmissionReview, pool *kihv1.IPPool) *admissionv1.AdmissionResponse {
	for _, vm := range pool.Status.IPv4.Allocated {
		if vm != "EXCLUDED" {
			return &admissionv1.AdmissionResponse{
				UID:     ar.Request.UID,
				Allowed: false,
				Result:  &metav1.Status{Message: "ippool is still in use"},
			}
		}
	}

	return &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}
}

func validateIPPoolAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)
	}

	pool := &kihv1.IPPool{}
	if err := json.Unmarshal(ar.Request.OldObject.Raw, &pool); err != nil {
		log.Errorf("cannot unmarshal json to pool: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot unmarshal json to pool: %s", err)
	}

	ar.Response = validateIPPool(ar, pool)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

func (h *Handler) Run() {
	homedir := os.Getenv("HOME")
	keyPath := fmt.Sprintf("%s/tls.key", homedir)
	certPath := fmt.Sprintf("%s/tls.crt", homedir)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/validate-ippool", validateIPPoolAdmission)

	h.httpServer = &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	log.Error(h.httpServer.ListenAndServeTLS(certPath, keyPath))
}

func (h *Handler) Stop() error {
	return h.httpServer.Shutdown(h.ctx)
}
