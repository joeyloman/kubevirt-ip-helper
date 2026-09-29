package config

// F12 regression tests: the webhook bootstraps of a rolling deployment
// share one csr name and one secret. pre-fix the issuance poll read the
// signing request by name only: a concurrent pod which deleted the
// request and created its own under the same name had its certificate
// retrieved by the first pod's poll and paired with the first pod's key,
// and the unusable pair was published as the shared secret - tls serving
// then failed repeatedly while startup kept reusing it.
//
// The fix verifies the polled object is the one this attempt created
// (uid and request bytes), validates the issued certificate against the
// generated key as one pair before anything is published, and makes the
// bootstrap's adoption of an already published secret explicit: the
// loser converges on the winner's credentials instead of deleting them.
//
// The fixtures drive the real typed clientset against a scripted
// apiserver which issues real certificates from a test ca for whatever
// public key the stored request carries, so a legitimate pair validates
// and a foreign one does not - the same property the production
// validation checks.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	certsv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// issuanceState is the scripted apiserver of the webhook config
// package: it stores the secret and the signing request objects the
// typed clientset creates and reads, issues a certificate for the
// public key of the stored request when the approval lands, and exposes
// the interference hooks of the concurrent-bootstrap scenarios.
type issuanceState struct {
	mu      sync.Mutex
	secrets map[string]*corev1.Secret
	csrs    map[string]*certsv1.CertificateSigningRequest
	uidSeq  int

	caKey  *rsa.PrivateKey
	caCert *x509.Certificate

	// replaceOnGet swaps the stored signing request with the given
	// object on the next read, the interleaving of a concurrent pod
	// which deleted the request and created its own under the same name
	replaceOnGet *certsv1.CertificateSigningRequest
	// issueForForeignKey makes the simulated signer issue the
	// certificate for a key which is not the request's: the identity
	// checks pass, only the pair validation can catch it
	issueForForeignKey bool
	foreignKey         *rsa.PrivateKey
	// preExistingSecretOnCreate answers the secret creation with
	// AlreadyExists and stores the given object first, the interleaving
	// of a concurrent bootstrap which published the shared secret
	// between this pod's no-secret read and its publication
	preExistingSecretOnCreate *corev1.Secret
}

func issuanceAPIServer(t *testing.T) (*issuanceState, *kubernetes.Clientset) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the test ca key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "issuance-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating the test ca: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing the test ca: %v", err)
	}
	foreignKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the foreign key: %v", err)
	}

	s := &issuanceState{
		secrets:    make(map[string]*corev1.Secret),
		csrs:       make(map[string]*certsv1.CertificateSigningRequest),
		caKey:      caKey,
		caCert:     caCert,
		foreignKey: foreignKey,
	}

	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("building clientset: %v", err)
	}

	return s, clientset
}

func (s *issuanceState) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/secrets"):
		s.createSecretRequest(w, r)
	case r.Method == http.MethodGet && strings.Contains(path, "/secrets/"):
		s.getSecretRequest(w, path)
	case r.Method == http.MethodDelete && strings.Contains(path, "/secrets/"):
		delete(s.secrets, nameFromPath(path))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/certificatesigningrequests"):
		s.createCSRRequest(w, r)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/approval"):
		s.approveCSRRequest(w, r)
	case r.Method == http.MethodGet && strings.Contains(path, "/certificatesigningrequests/"):
		s.getCSRRequest(w, path)
	case r.Method == http.MethodDelete && strings.Contains(path, "/certificatesigningrequests/"):
		delete(s.csrs, nameFromPath(strings.TrimSuffix(path, "/approval")))
		w.WriteHeader(http.StatusOK)
	default:
		s.writeStatus(w, http.StatusNotFound, "NotFound", fmt.Sprintf("no route for %s %s", r.Method, path))
	}
}

func nameFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")

	return parts[len(parts)-1]
}

func (s *issuanceState) writeStatus(w http.ResponseWriter, code int32, reason string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(int(code))
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Code:     code,
		Reason:   metav1.StatusReason(reason),
		Message:  message,
	})
}

func (s *issuanceState) writeObject(w http.ResponseWriter, code int, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}

func (s *issuanceState) createSecretRequest(w http.ResponseWriter, r *http.Request) {
	secret := &corev1.Secret{}
	if err := json.NewDecoder(r.Body).Decode(secret); err != nil {
		s.writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())

		return
	}

	if s.preExistingSecretOnCreate != nil {
		winner := s.preExistingSecretOnCreate
		s.preExistingSecretOnCreate = nil
		s.secrets[secret.Name] = winner
		s.writeStatus(w, http.StatusConflict, "AlreadyExists",
			fmt.Sprintf("secrets %q already exists", secret.Name))

		return
	}

	if _, exists := s.secrets[secret.Name]; exists {
		s.writeStatus(w, http.StatusConflict, "AlreadyExists",
			fmt.Sprintf("secrets %q already exists", secret.Name))

		return
	}

	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	s.secrets[secret.Name] = secret
	s.writeObject(w, http.StatusCreated, secret)
}

func (s *issuanceState) getSecretRequest(w http.ResponseWriter, path string) {
	secret, exists := s.secrets[nameFromPath(path)]
	if !exists {
		s.writeStatus(w, http.StatusNotFound, "NotFound",
			fmt.Sprintf("secrets %q not found", nameFromPath(path)))

		return
	}

	s.writeObject(w, http.StatusOK, secret)
}

func (s *issuanceState) createCSRRequest(w http.ResponseWriter, r *http.Request) {
	csr := &certsv1.CertificateSigningRequest{}
	if err := json.NewDecoder(r.Body).Decode(csr); err != nil {
		s.writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())

		return
	}

	if _, exists := s.csrs[csr.Name]; exists {
		s.writeStatus(w, http.StatusConflict, "AlreadyExists",
			fmt.Sprintf("certificatesigningrequests %q already exists", csr.Name))

		return
	}

	s.uidSeq++
	csr.UID = types.UID(fmt.Sprintf("uid-%s-%d", csr.Name, s.uidSeq))
	csr.TypeMeta = metav1.TypeMeta{APIVersion: "certificates.k8s.io/v1", Kind: "CertificateSigningRequest"}
	s.csrs[csr.Name] = csr
	s.writeObject(w, http.StatusCreated, csr)
}

func (s *issuanceState) approveCSRRequest(w http.ResponseWriter, r *http.Request) {
	name := nameFromPath(strings.TrimSuffix(r.URL.Path, "/approval"))
	approval := &certsv1.CertificateSigningRequest{}
	if err := json.NewDecoder(r.Body).Decode(approval); err != nil {
		s.writeStatus(w, http.StatusBadRequest, "BadRequest", err.Error())

		return
	}

	stored, exists := s.csrs[name]
	if !exists {
		s.writeStatus(w, http.StatusNotFound, "NotFound",
			fmt.Sprintf("certificatesigningrequests %q not found", name))

		return
	}

	stored.Status.Conditions = approval.Status.Conditions

	certPEM, err := s.issueForRequest(stored.Spec.Request)
	if err != nil {
		s.writeStatus(w, http.StatusInternalServerError, "InternalError", err.Error())

		return
	}
	stored.Status.Certificate = certPEM

	s.writeObject(w, http.StatusOK, stored)
}

func (s *issuanceState) getCSRRequest(w http.ResponseWriter, path string) {
	name := nameFromPath(path)

	if s.replaceOnGet != nil {
		// the concurrent pod deleted this object and created its own
		// under the same name between this attempt's create and its
		// first poll read
		replacement := s.replaceOnGet
		s.replaceOnGet = nil
		s.csrs[name] = replacement
	}

	csr, exists := s.csrs[name]
	if !exists {
		s.writeStatus(w, http.StatusNotFound, "NotFound",
			fmt.Sprintf("certificatesigningrequests %q not found", name))

		return
	}

	s.writeObject(w, http.StatusOK, csr)
}

// issueForRequest builds a certificate for the public key the given
// request PEM carries, signed by the test ca: a pair assembled from it
// and the request's key validates, a pair assembled from it and any
// other key does not.
func (s *issuanceState) issueForRequest(requestPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(requestPEM)
	if block == nil {
		return nil, fmt.Errorf("the stored request is not PEM encoded")
	}
	csrReq, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the stored request: %s", err.Error())
	}

	pub := crypto.PublicKey(csrReq.PublicKey)
	if s.issueForForeignKey {
		pub = &s.foreignKey.PublicKey
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: csrReq.Subject.CommonName},
		DNSNames:     csrReq.DNSNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, s.caCert, pub, s.caKey)
	if err != nil {
		return nil, fmt.Errorf("issuing the certificate: %s", err.Error())
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// foreignSignedCSR builds the signing request object of the concurrent
// pod: its own key material under the shared name, already approved and
// carrying a certificate for its own key.
func (s *issuanceState) foreignSignedCSR(t *testing.T, name string) *certsv1.CertificateSigningRequest {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the concurrent key: %v", err)
	}
	template := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "system:node:concurrent"},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	bCsr, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatalf("creating the concurrent request: %v", err)
	}
	requestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: bCsr})

	certPEM, err := s.issueForRequest(requestPEM)
	if err != nil {
		t.Fatalf("issuing the concurrent certificate: %v", err)
	}

	return &certsv1.CertificateSigningRequest{
		TypeMeta:   metav1.TypeMeta{APIVersion: "certificates.k8s.io/v1", Kind: "CertificateSigningRequest"},
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-concurrent-attempt"},
		Spec:       certsv1.CertificateSigningRequestSpec{Request: requestPEM},
		Status: certsv1.CertificateSigningRequestStatus{
			Conditions:  []certsv1.CertificateSigningRequestCondition{{Type: certsv1.CertificateApproved, Status: corev1.ConditionTrue}},
			Certificate: certPEM,
		},
	}
}

// validForeignSecret builds the secret the concurrent bootstrap
// published: a usable key/certificate pair which is not this pod's.
func (s *issuanceState) validForeignSecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the winner key: %v", err)
	}
	pemKey, err := pemEncodePKCS8Key(key)
	if err != nil {
		t.Fatalf("encoding the winner key: %v", err)
	}

	requestTemplate := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "system:node:winner"},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	bCsr, err := x509.CreateCertificateRequest(rand.Reader, requestTemplate, key)
	if err != nil {
		t.Fatalf("creating the winner request: %v", err)
	}
	requestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: bCsr})
	certPEM, err := s.issueForRequest(requestPEM)
	if err != nil {
		t.Fatalf("issuing the winner certificate: %v", err)
	}

	// the winner's pair must be usable, like the pair the real
	// concurrent bootstrap would have published
	if _, err := tls.X509KeyPair(certPEM, pemKey); err != nil {
		t.Fatalf("the winner pair does not validate: %v", err)
	}

	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Type:       "kubernetes.io/tls",
		Data: map[string][]byte{
			"tls.key": pemKey,
			"tls.crt": certPEM,
		},
	}
}

func issuanceHandler(clientset *kubernetes.Clientset) *Handler {
	return &Handler{
		ctx:               context.Background(),
		clientset:         clientset,
		webhookName:       "webhook",
		webhookNamespace:  "test",
		webhookSecretName: "webhook-tls",
		csrName:           "webhook.test.svc",
	}
}

// TestIssuanceAbortsWhenTheRequestIsReplaced: the review's interleaving
// - the concurrent pod deletes the shared-name signing request and
// creates its own between this attempt's create and its first poll
// read. the poll must not pair the concurrent attempt's certificate
// with this attempt's key: the issuance aborts and nothing is
// published, so the losing pod restarts into adopting the winner.
func TestIssuanceAbortsWhenTheRequestIsReplaced(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)
	s.replaceOnGet = s.foreignSignedCSR(t, h.csrName)

	_, err := h.generateTLSKeyAndCert()
	if err == nil {
		t.Fatal("the issuance returned a pair although the signing request was replaced under the shared name")
	}
	if !strings.Contains(err.Error(), "replaced by a concurrent issuance attempt") {
		t.Errorf("the abort reason = %q, want the concurrent-replacement classification", err.Error())
	}

	// nothing was published: the loser must not persist a pair it
	// cannot vouch for
	if len(s.secrets) != 0 {
		t.Errorf("the aborted bootstrap published %d secret(s)", len(s.secrets))
	}
}

// TestIssuanceValidatesThePairBeforePublication pins the usability
// check behind the identity checks: a signer which issues the
// certificate for the wrong key (or any defect which slips a foreign
// certificate past the request verification) must fail the bootstrap
// instead of publishing a pair which cannot serve.
func TestIssuanceValidatesThePairBeforePublication(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)
	s.issueForForeignKey = true

	_, err := h.generateTLSKeyAndCert()
	if err == nil {
		t.Fatal("the issuance returned a pair whose certificate does not carry the generated key")
	}
	if !strings.Contains(err.Error(), "does not pair with the generated key") {
		t.Errorf("the abort reason = %q, want the pair-validation classification", err.Error())
	}

	if len(s.secrets) != 0 {
		t.Errorf("the aborted bootstrap published %d secret(s)", len(s.secrets))
	}
}

// TestBootstrapAdoptsTheSecretOfTheConcurrentWinner: both pods read no
// secret, both issue their own valid pair, and the concurrent pod
// publishes first. the loser's publication conflicts, and the bootstrap
// adopts the winner's pair instead of deleting it: the shared secret
// keeps the winner's data and the loser serves it.
func TestBootstrapAdoptsTheSecretOfTheConcurrentWinner(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)
	s.preExistingSecretOnCreate = s.validForeignSecret(t, h.webhookSecretName)

	home := t.TempDir()
	t.Setenv("HOME", home)

	h.Run(0)

	// the winner's secret was adopted, not deleted or overwritten
	winner, exists := s.secrets[h.webhookSecretName]
	if !exists {
		t.Fatal("the winner's secret was deleted by the losing bootstrap")
	}
	winnerCert := winner.Data["tls.crt"]
	winnerKey := winner.Data["tls.key"]

	certBytes, err := os.ReadFile(home + "/tls.crt")
	if err != nil {
		t.Fatalf("reading the written certificate: %v", err)
	}
	if string(certBytes) != string(winnerCert) {
		t.Error("the loser does not serve the winner's certificate")
	}
	keyBytes, err := os.ReadFile(home + "/tls.key")
	if err != nil {
		t.Fatalf("reading the written key: %v", err)
	}
	if string(keyBytes) != string(winnerKey) {
		t.Error("the loser does not serve the winner's key")
	}
}
