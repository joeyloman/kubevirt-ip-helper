package config

// F13 regression tests: the certificate replacement deleted the secret
// before creating its replacement, so a successful issuance followed by
// a failed creation left no persisted usable pair at all - no restart
// and no renewal retry could recover it. the loaded pair was never
// validated either: garbage PEM panicked in the expiry parse through a
// nil-error dereference, malformed data was classified as not due for
// renewal, and an unexpired certificate paired with the wrong key was
// never repaired, while the file writer installed whatever the secret
// held as the serving credentials.
//
// The replacement is now one resource-version-aware update which keeps
// the previous pair until it succeeds, the loaded pair must validate
// before anything serves or decides on it, an unusable pair is due for
// the replacement immediately, and the serving files are only written
// from a usable pair.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// seedSecret installs a secret in the scripted apiserver's store.
func seedSecret(s *issuanceState, secret *corev1.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.secrets[secret.Name] = secret
}

func storedSecret(s *issuanceState, name string) *corev1.Secret {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.secrets[name]
}

// TestRenewalKeepsTheLastGoodPairWhenTheReplacementFails: the issuance
// succeeds but the secret update fails. the previous delete/create
// sequence had already destroyed the persisted pair at that point, so
// no restart could recover it; the resource-version-aware update keeps
// the last-good pair in place, and the retried renewal replaces it.
func TestRenewalKeepsTheLastGoodPairWhenTheReplacementFails(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)

	seeded := s.validForeignSecret(t, h.webhookSecretName)
	seeded.ResourceVersion = "rv-1"
	seedSecret(s, seeded)
	oldCert := string(seeded.Data["tls.crt"])
	oldKey := string(seeded.Data["tls.key"])

	s.mu.Lock()
	s.failSecretUpdate = true
	s.mu.Unlock()

	if err := h.renewTLSPair(); err == nil {
		t.Fatal("the renewal succeeded although the secret replacement failed")
	}

	// the last-good pair is untouched: the failed replacement destroyed
	// nothing
	stored := storedSecret(s, h.webhookSecretName)
	if stored == nil {
		t.Fatal("the failed replacement deleted the persisted pair")
	}
	if string(stored.Data["tls.crt"]) != oldCert || string(stored.Data["tls.key"]) != oldKey {
		t.Error("the failed replacement modified the last-good pair")
	}

	s.mu.Lock()
	s.failSecretUpdate = false
	s.mu.Unlock()

	if err := h.renewTLSPair(); err != nil {
		t.Fatalf("the retried renewal: %s", err)
	}

	// the retry replaced the pair, resource-version aware, and the
	// replacement validates
	stored = storedSecret(s, h.webhookSecretName)
	if string(stored.Data["tls.crt"]) == oldCert {
		t.Error("the retried renewal did not replace the certificate")
	}
	if _, err := tls.X509KeyPair(stored.Data["tls.crt"], stored.Data["tls.key"]); err != nil {
		t.Errorf("the replaced pair does not validate: %v", err)
	}
	s.mu.Lock()
	carriedRV := s.lastUpdateRV
	s.mu.Unlock()
	if carriedRV != "rv-1" {
		t.Errorf("the replacement carried resource version %q, want the stored rv-1 (resource-version aware)", carriedRV)
	}
}

// TestUnusablePersistedPairIsRepairedByTheRenewal: a persisted secret
// with garbage certificate data. pre-fix the expiry parse panicked on
// it through a nil-error dereference, and even without the panic the
// data was classified as not due for renewal, so the garbage was served
// forever and no restart healed it (the secret survives restarts). the
// unusable pair must be due for the replacement immediately, and the
// renewal must replace it with a validating pair which reaches the
// serving files.
func TestUnusablePersistedPairIsRepairedByTheRenewal(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)
	home := t.TempDir()
	t.Setenv("HOME", home)

	seedSecret(s, &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: h.webhookSecretName, ResourceVersion: "rv-1"},
		Type:       "kubernetes.io/tls",
		Data: map[string][]byte{
			"tls.crt": []byte("this is not a pem certificate"),
			"tls.key": []byte("this is not a pem key"),
		},
	})

	// classifying the unusable pair must not panic and must schedule the
	// replacement
	if !h.checkCertExpireDate(60 * 24 * 365) {
		t.Fatal("the unusable persisted pair must be due for the replacement")
	}

	if err := h.Run(60 * 24 * 365); err != nil {
		t.Fatalf("the repairing renewal: %s", err)
	}

	stored := storedSecret(s, h.webhookSecretName)
	if _, err := tls.X509KeyPair(stored.Data["tls.crt"], stored.Data["tls.key"]); err != nil {
		t.Errorf("the repaired pair does not validate: %v", err)
	}

	certBytes, err := os.ReadFile(home + "/tls.crt")
	if err != nil {
		t.Fatalf("reading the written certificate: %v", err)
	}
	keyBytes, err := os.ReadFile(home + "/tls.key")
	if err != nil {
		t.Fatalf("reading the written key: %v", err)
	}
	if _, err := tls.X509KeyPair(certBytes, keyBytes); err != nil {
		t.Errorf("the written serving pair does not validate: %v", err)
	}
	if string(certBytes) == "this is not a pem certificate" {
		t.Error("the garbage certificate was written as the serving credential")
	}
}

// TestUnusablePairsAreClassifiedNotServed: the bad-data variants of the
// review - empty data, garbage PEM, a well-formed PEM block with a
// malformed DER body, and a valid certificate paired with the wrong
// key. each must be classified as unusable (a controlled error, no
// panic), must be due for the replacement, and must not be installed as
// the serving credentials: the write refuses and the previously good
// files survive.
func TestUnusablePairsAreClassifiedNotServed(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)

	goodKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the good key: %v", err)
	}
	goodKeyPEM, err := pemEncodePKCS8Key(goodKey)
	if err != nil {
		t.Fatalf("encoding the good key: %v", err)
	}
	goodRequest := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "system:node:good"},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	goodCsrDER, err := x509.CreateCertificateRequest(rand.Reader, goodRequest, goodKey)
	if err != nil {
		t.Fatalf("creating the good request: %v", err)
	}
	goodRequestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: goodCsrDER})
	goodCertPEM, err := s.issueForRequest(goodRequestPEM)
	if err != nil {
		t.Fatalf("issuing the good certificate: %v", err)
	}

	// a valid certificate for another key, paired with the good key
	foreignCertPEM, err := s.certForAnotherKey(t)
	if err != nil {
		t.Fatalf("issuing the foreign certificate: %v", err)
	}

	cases := []struct {
		name string
		crt  []byte
		key  []byte
	}{
		{"empty certificate data", nil, goodKeyPEM},
		{"garbage pem", []byte("this is not a pem certificate"), goodKeyPEM},
		{"malformed der", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("this is not a der body")}), goodKeyPEM},
		{"valid certificate with the wrong key", foreignCertPEM, goodKeyPEM},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedSecret(s, &corev1.Secret{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
				ObjectMeta: metav1.ObjectMeta{Name: h.webhookSecretName, ResourceVersion: "rv-1"},
				Type:       "kubernetes.io/tls",
				Data: map[string][]byte{
					"tls.crt": tc.crt,
					"tls.key": tc.key,
				},
			})

			// the classification is a controlled error carrying the
			// unusable-pair verdict, never a panic
			_, err := h.GetCertExpireDate()
			if err == nil {
				t.Fatalf("%s: the expiry of an unusable pair must be an error", tc.name)
			}
			if !errors.Is(err, errUnusableTLSPair) {
				t.Errorf("%s: the error = %v, want the unusable-pair classification", tc.name, err)
			}

			if !h.checkCertExpireDate(60 * 24 * 365) {
				t.Errorf("%s: the unusable pair must be due for the replacement", tc.name)
			}

			// the write refuses the unusable data and the previously
			// good serving files survive it
			home := t.TempDir()
			t.Setenv("HOME", home)
			if err := os.WriteFile(home+"/tls.key", goodKeyPEM, 0600); err != nil {
				t.Fatalf("%s: seeding the good key file: %v", tc.name, err)
			}
			if err := os.WriteFile(home+"/tls.crt", goodCertPEM, 0644); err != nil {
				t.Fatalf("%s: seeding the good certificate file: %v", tc.name, err)
			}

			if err := h.writeTLSDataFromSecret(); err == nil {
				t.Errorf("%s: writing an unusable pair must fail", tc.name)
			}

			certBytes, err := os.ReadFile(home + "/tls.crt")
			if err != nil || string(certBytes) != string(goodCertPEM) {
				t.Errorf("%s: the good certificate file was replaced: err=%v equal=%t", tc.name, err, string(certBytes) == string(goodCertPEM))
			}
			keyBytes, err := os.ReadFile(home + "/tls.key")
			if err != nil || string(keyBytes) != string(goodKeyPEM) {
				t.Errorf("%s: the good key file was replaced: err=%v equal=%t", tc.name, err, string(keyBytes) == string(goodKeyPEM))
			}
		})
	}
}

// TestValidNotDuePairIsLeftAlone: the counterpart of the classification
// - a validating pair whose expiry is far away is not due, so the
// renewal does not touch it.
func TestValidNotDuePairIsLeftAlone(t *testing.T) {
	s, clientset := issuanceAPIServer(t)
	h := issuanceHandler(clientset)

	seeded := s.validForeignSecret(t, h.webhookSecretName)
	seeded.ResourceVersion = "rv-1"
	seedSecret(s, seeded)

	if h.checkCertExpireDate(1) {
		t.Error("a valid pair expiring in the far future must not be due for the renewal")
	}
}

// issueForForeignKey issues a certificate for a key which is not the
// request's, through the same signer machinery: paired with any other
// key it forms the valid-certificate-wrong-key variant.
func (s *issuanceState) certForAnotherKey(t *testing.T) ([]byte, error) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating the foreign key: %s", err.Error())
	}
	request := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "system:node:foreign"},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, request, key)
	if err != nil {
		return nil, fmt.Errorf("creating the foreign request: %s", err.Error())
	}
	requestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	s.issueForForeignKey = true
	certPEM, err := s.issueForRequest(requestPEM)
	s.issueForForeignKey = false
	if err != nil {
		return nil, err
	}

	return certPEM, nil
}
