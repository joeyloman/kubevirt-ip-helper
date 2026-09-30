package config

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"

	certsv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (h *Handler) generateTLSKeyAndCert() (tlsPair tls.Certificate, err error) {
	var DNSnames []string

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tlsPair, fmt.Errorf("error while generating key: %s", err.Error())
	}

	cn := fmt.Sprintf("system:node:%s.%s.svc", h.webhookName, h.webhookNamespace)
	DNSnames = append(DNSnames, h.webhookName)
	DNSnames = append(DNSnames, fmt.Sprintf("%s.%s", h.webhookName, h.webhookNamespace))
	DNSnames = append(DNSnames, h.csrName)
	DNSnames = append(DNSnames, fmt.Sprintf("%s.%s.cluster.local", h.webhookName, h.webhookNamespace))

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"system:nodes"},
		},
		SignatureAlgorithm: x509.SHA256WithRSA,
		DNSNames:           DNSnames,
	}

	bCsr, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return tlsPair, fmt.Errorf("error while creating certificate request: %s", err.Error())
	}
	pCsr := pem.EncodeToMemory(
		&pem.Block{
			Type:  "CERTIFICATE REQUEST",
			Bytes: bCsr,
		},
	)

	cert, err := h.createAndSignCSR(pCsr)
	if err != nil {
		return
	}

	tlsPair.Certificate = append(tlsPair.Certificate, cert)
	tlsPair.PrivateKey = key

	return
}

func (h *Handler) checkCSR() bool {
	_, err := h.getCSR()
	return err == nil
}

func (h *Handler) getCSR() (*certsv1.CertificateSigningRequest, error) {
	return h.clientset.CertificatesV1().CertificateSigningRequests().Get(context.TODO(), h.csrName, metav1.GetOptions{})
}

func (h *Handler) deleteCSR() error {
	return h.clientset.CertificatesV1().CertificateSigningRequests().Delete(context.TODO(), h.csrName, metav1.DeleteOptions{})
}

func (h *Handler) createAndSignCSR(pCsr []byte) ([]byte, error) {
	newCsrObj := certsv1.CertificateSigningRequest{}
	newCsrObj.ObjectMeta.Name = h.csrName
	newCsrObj.Spec.Groups = []string{"system:authenticated"}
	newCsrObj.Spec.Request = pCsr
	newCsrObj.Spec.SignerName = "kubernetes.io/kubelet-serving"
	newCsrObj.Spec.Usages = []certsv1.KeyUsage{
		certsv1.UsageDigitalSignature,
		certsv1.UsageKeyEncipherment,
		certsv1.UsageServerAuth,
	}
	csrObj, err := h.clientset.CertificatesV1().CertificateSigningRequests().Create(context.TODO(), &newCsrObj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error while creating signing request: %s", err.Error())
	}

	approval := certsv1.CertificateSigningRequest{
		Status: certsv1.CertificateSigningRequestStatus{
			Conditions: []certsv1.CertificateSigningRequestCondition{{
				Type:           certsv1.CertificateApproved,
				Status:         corev1.ConditionTrue,
				Reason:         "Approved by TLS Service",
				Message:        "KubeTLS Approved",
				LastUpdateTime: metav1.Now(),
			}},
		},
	}
	approval.ObjectMeta = csrObj.ObjectMeta
	_, err = h.clientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(context.TODO(), h.csrName, &approval, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error while approving signing request: %s", err.Error())
	}

	time.Sleep(2 * time.Second)

	updatedCsr, err := h.clientset.CertificatesV1().CertificateSigningRequests().Get(context.TODO(), h.csrName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error while getting the updated signing request: %s", err.Error())
	}

	return updatedCsr.Status.Certificate, nil
}

func (h *Handler) getTLSDataFromSecret() (tlsPair tls.Certificate, err error) {
	s := h.getSecret()

	cert, exists := s.Data["tls.crt"]
	if !exists {
		return tlsPair, fmt.Errorf("tls.crt not found in secret")
	}
	tlsPair.Certificate = append(tlsPair.Certificate, cert)

	key, exists := s.Data["tls.key"]
	if !exists {
		return tlsPair, fmt.Errorf("tls.key not found in secret")
	}
	tlsPair.PrivateKey = key

	return
}

func (h *Handler) writeTLSDataFromSecret() (err error) {
	homedir := os.Getenv("HOME")
	keyPath := fmt.Sprintf("%s/tls.key", homedir)
	certPath := fmt.Sprintf("%s/tls.crt", homedir)

	tlsPair, err := h.getTLSDataFromSecret()
	if err != nil {
		return fmt.Errorf("cannot while fetching TLS data: %s", err.Error())
	}

	if err = os.WriteFile(keyPath, []byte(fmt.Sprintf("%s", tlsPair.PrivateKey)), 0600); err != nil {
		return fmt.Errorf("error while writing private key file: %s", err.Error())
	}

	if err = os.WriteFile(certPath, tlsPair.Certificate[0], 0644); err != nil {
		return fmt.Errorf("error while writing certificate file: %s", err.Error())
	}

	return
}

func (h *Handler) renewTLSPair() (err error) {
	if h.checkCSR() {
		if err = h.deleteCSR(); err != nil {
			return
		}
	}

	tlsPair, err := h.generateTLSKeyAndCert()
	if err != nil {
		return
	}

	if err = h.deleteSecret(); err != nil {
		return
	}

	return h.createSecret(tlsPair)
}

func (h *Handler) GetCertExpireDate() (expireDate time.Time, err error) {
	tlsPair, err := h.getTLSDataFromSecret()
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot while fetching TLS data: %s", err.Error())
	}

	if len(tlsPair.Certificate[0]) == 0 {
		return time.Time{}, fmt.Errorf("certificate is empty")
	}
	b, _ := pem.Decode(tlsPair.Certificate[0])
	if b == nil {
		return time.Time{}, fmt.Errorf("cannot decode TLS PEM data: %s", err.Error())
	}

	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse TLS PEM data: %s", err.Error())
	}

	return cert.NotAfter, err
}

func (h *Handler) checkCertExpireDate(certRenewalPeriod int64) bool {
	expireDate, err := h.GetCertExpireDate()
	if err != nil {
		log.Errorf("%s", err.Error())

		return false
	}

	currentDate := time.Now().UTC()
	difference := expireDate.Sub(currentDate)
	return int64(difference.Minutes()) < certRenewalPeriod
}
