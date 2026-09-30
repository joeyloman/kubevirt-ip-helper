package admission

import (
	"context"
	"fmt"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
	admregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Handler struct {
	ctx                         context.Context
	kubeConfig                  string
	kubeContext                 string
	clientset                   *kubernetes.Clientset
	webhookNamespace            string
	webhookName                 string
	validatingWebhookConfigName string
}

func Register(ctx context.Context, kubeConfig string, kubeContext string, webhookName string, webhookNamespace string, validatingWebhookConfigName string) *Handler {
	return &Handler{
		ctx:                         ctx,
		kubeConfig:                  kubeConfig,
		kubeContext:                 kubeContext,
		webhookName:                 webhookName,
		webhookNamespace:            webhookNamespace,
		validatingWebhookConfigName: validatingWebhookConfigName,
	}
}

func (h *Handler) Init() {
	config, err := util.GetKubeConfig(h.kubeConfig, h.kubeContext)
	if err != nil {
		log.Panicf("%s", err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panicf("%s", err.Error())
	}
	h.clientset = clientset

	if err := h.AddValidatingWebhookConfiguration(); err != nil {
		log.Panicf("%s", err.Error())
	}
}

func (h *Handler) checkValidatingWebhookConfiguration() bool {
	_, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), h.validatingWebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	return true
}

func (h *Handler) AddValidatingWebhookConfiguration() (err error) {
	if h.checkValidatingWebhookConfiguration() {
		return
	}

	cert, err := h.getCaBundleFromCABundleConfigMap()
	if err != nil {
		return
	}

	vwc := admregv1.ValidatingWebhookConfiguration{}
	vwc.ObjectMeta.Name = h.validatingWebhookConfigName

	webhook := admregv1.ValidatingWebhook{}
	webhook.Name = fmt.Sprintf("%s.%s.svc", h.webhookName, h.webhookNamespace)

	matchLabels := make(map[string]string)
	matchLabels["admission-webhook"] = "enabled"
	nameSpaceSelector := metav1.LabelSelector{}
	nameSpaceSelector.MatchLabels = matchLabels
	webhook.NamespaceSelector = &nameSpaceSelector

	var rules []admregv1.RuleWithOperations
	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"kubevirtiphelper.k8s.binbash.org"}
	rule.APIVersions = []string{"v1"}
	rule.Operations = []admregv1.OperationType{"DELETE"}
	rule.Resources = []string{"ippools"}
	scope := admregv1.AllScopes
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	clientconfig := admregv1.WebhookClientConfig{}
	serviceref := admregv1.ServiceReference{}
	serviceref.Namespace = h.webhookNamespace
	serviceref.Name = h.webhookName
	path := "/validate-ippool"
	serviceref.Path = &path
	port := int32(8080)
	serviceref.Port = &port
	clientconfig.Service = &serviceref
	clientconfig.CABundle = []byte(cert)
	webhook.ClientConfig = clientconfig

	webhook.AdmissionReviewVersions = []string{"v1"}

	vwc.Webhooks = append(vwc.Webhooks, webhook)

	_, err = h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), &vwc, metav1.CreateOptions{})

	return
}
