package util

import (
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// GetKubeConfig returns the rest config for the given kubeconfig file and
// context, falling back to the in-cluster config when the file does not
// exist (the webhook and controller kubeconfig-detection paths share this
// behavior: an unreadable kubeconfig resolves to the in-cluster config
// rather than to a hard failure).
func GetKubeConfig(kubeConfig string, kubeContext string) (config *rest.Config, err error) {
	if !FileExists(kubeConfig) {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: kubeContext},
	).ClientConfig()
}

// WatchRestConfig strips the one-shot client timeout for the informer
// client: the timeout applies to the watch connections too, so the
// reflector's long-poll would be torn down by the http client every time
// it expires (a constant re-watch churn), and an initial list which takes
// longer than the timeout would never complete, leaving the controller
// blocked in the cache sync wait. the one-shot bound stays on the config
// handed to the one-shot clientsets.
func WatchRestConfig(config *rest.Config) *rest.Config {
	watchConfig := rest.CopyConfig(config)
	watchConfig.Timeout = 0

	return watchConfig
}
