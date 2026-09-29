package admission

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (h *Handler) getCABundleConfigMap(ctx context.Context) (c corev1.ConfigMap, err error) {
	// a failed read of an absent configmap is an acceptable state and
	// reads as an empty one, but a canceled or expired read context is
	// not: the caller's context error must surface, so an aborted
	// registration is reported as an abort instead of a mysteriously
	// missing ca bundle (F08)
	configmap, err := h.clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}

		return c, nil
	}

	return *configmap, nil
}
