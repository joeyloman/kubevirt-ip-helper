package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (h *Handler) createSecret(tlsPair tls.Certificate) (err error) {
	bKey, err := x509.MarshalPKCS8PrivateKey(tlsPair.PrivateKey)
	if err != nil {
		return fmt.Errorf("unable to marshal private key: %s", err.Error())

	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: bKey})
	if pemKey == nil {
		return fmt.Errorf("failed to encode key to PEM")
	}

	newSecret := corev1.Secret{}
	newSecret.Type = "kubernetes.io/tls"
	newSecret.ObjectMeta.Name = h.webhookSecretName
	newSecret.ObjectMeta.Namespace = h.webhookNamespace
	secretData := make(map[string][]byte)
	secretData["tls.key"] = pemKey
	secretData["tls.crt"] = tlsPair.Certificate[0]
	newSecret.Data = secretData

	_, err = h.clientset.CoreV1().Secrets(h.webhookNamespace).Create(context.TODO(), &newSecret, metav1.CreateOptions{})

	return
}

func (h *Handler) getSecret() corev1.Secret {
	secret, err := h.clientset.CoreV1().Secrets(h.webhookNamespace).Get(context.TODO(), h.webhookSecretName, metav1.GetOptions{})
	if err != nil {
		return corev1.Secret{}
	}

	return *secret
}

func (h *Handler) deleteSecret() (err error) {
	err = h.clientset.CoreV1().Secrets(h.webhookNamespace).Delete(context.TODO(), h.webhookSecretName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("cannot delete webhook secret: %s", err.Error())
	}

	return
}

func (h *Handler) checkSecret() bool {
	s := h.getSecret()

	return s.ObjectMeta.Name != ""
}
