package config

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// the one-shot secret calls run on the handler's process context (F08):
// it is canceled when the process shuts down, and every call is
// additionally bounded by the request timeout of the kubeconfig.

// pemEncodePKCS8Key marshals a private key to the PEM spelling the
// secret publication and the pair validation share.
func pemEncodePKCS8Key(key crypto.PrivateKey) ([]byte, error) {
	bKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal private key: %s", err.Error())
	}

	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: bKey})
	if pemKey == nil {
		return nil, fmt.Errorf("failed to encode key to PEM")
	}

	return pemKey, nil
}

func (h *Handler) createSecret(tlsPair tls.Certificate) (err error) {
	pemKey, err := pemEncodePKCS8Key(tlsPair.PrivateKey)
	if err != nil {
		return err
	}

	newSecret := corev1.Secret{}
	newSecret.Type = "kubernetes.io/tls"
	newSecret.ObjectMeta.Name = h.webhookSecretName
	newSecret.ObjectMeta.Namespace = h.webhookNamespace
	secretData := make(map[string][]byte)
	secretData["tls.key"] = pemKey
	secretData["tls.crt"] = tlsPair.Certificate[0]
	newSecret.Data = secretData

	_, err = h.clientset.CoreV1().Secrets(h.webhookNamespace).Create(h.ctx, &newSecret, metav1.CreateOptions{})

	return
}

func (h *Handler) getSecret() corev1.Secret {
	secret, err := h.clientset.CoreV1().Secrets(h.webhookNamespace).Get(h.ctx, h.webhookSecretName, metav1.GetOptions{})
	if err != nil {
		return corev1.Secret{}
	}

	return *secret
}

func (h *Handler) deleteSecret() (err error) {
	err = h.clientset.CoreV1().Secrets(h.webhookNamespace).Delete(h.ctx, h.webhookSecretName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("cannot delete webhook secret: %s", err.Error())
	}

	return
}

func (h *Handler) checkSecret() bool {
	s := h.getSecret()

	return s.ObjectMeta.Name != ""
}
