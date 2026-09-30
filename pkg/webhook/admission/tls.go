package admission

import "fmt"

func (h *Handler) getCaBundleFromCABundleConfigMap() (cert string, err error) {
	c := h.getCABundleConfigMap()

	cert, exists := c.Data["ca.crt"]
	if !exists {
		return cert, fmt.Errorf("ca.crt not found in configmap")
	}

	return cert, err
}
