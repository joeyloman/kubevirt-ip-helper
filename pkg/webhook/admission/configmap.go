package admission

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (h *Handler) getCABundleConfigMap() corev1.ConfigMap {
	configmap, err := h.clientset.CoreV1().ConfigMaps("kube-system").Get(context.TODO(), "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return corev1.ConfigMap{}
	}

	return *configmap
}
