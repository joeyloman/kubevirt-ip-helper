package admission

import (
	"context"
	"fmt"
)

func (h *Handler) getCaBundleFromCABundleConfigMap(ctx context.Context) (cert string, err error) {
	c, err := h.getCABundleConfigMap(ctx)
	if err != nil {
		return cert, err
	}

	cert, exists := c.Data["ca.crt"]
	if !exists {
		return cert, fmt.Errorf("ca.crt not found in configmap")
	}

	return cert, err
}
