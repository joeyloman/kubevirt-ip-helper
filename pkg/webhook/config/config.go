package config

import (
	"context"
	"fmt"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

type Handler struct {
	ctx               context.Context
	kubeConfig        string
	kubeContext       string
	clientset         *kubernetes.Clientset
	webhookNamespace  string
	webhookName       string
	webhookSecretName string
	csrName           string
}

func Register(ctx context.Context, kubeConfig string, kubeContext string, webhookName string, webhookNamespace string) *Handler {
	return &Handler{
		ctx:              ctx,
		kubeConfig:       kubeConfig,
		kubeContext:      kubeContext,
		webhookName:      webhookName,
		webhookNamespace: webhookNamespace,
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

	h.webhookSecretName = fmt.Sprintf("%s-tls", h.webhookName)
	h.csrName = fmt.Sprintf("%s.%s.svc", h.webhookName, h.webhookNamespace)
}

func (h *Handler) Run(certRenewalPeriod int64) {
	if h.checkSecret() {
		if h.checkCertExpireDate(certRenewalPeriod) {
			if err := h.renewTLSPair(); err != nil {
				log.Errorf("%s", err.Error())
			}
		}
	} else {
		if h.checkCSR() {
			if err := h.deleteCSR(); err != nil {
				log.Errorf("%s", err.Error())
			}
		}

		tlsPair, err := h.generateTLSKeyAndCert()
		if err != nil {
			// without a usable key/cert pair there is nothing to store:
			// createSecret would panic on the empty certificate, so a
			// generation failure (transient apiserver error, keygen
			// failure) must abort here and let the pod restart retry
			log.Fatalf("(webhook.config) %s", err.Error())
		}

		if err := h.createSecret(tlsPair); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// a concurrent bootstrap published the shared secret first
				// (F12): adopt its pair instead of deleting it - the load
				// below writes and serves the winner's credentials, so the
				// loser converges on them
				log.Infof("(webhook.config) the webhook secret %s was already published by a concurrent bootstrap, adopting it",
					h.webhookSecretName)
			} else {
				log.Errorf("%s", err.Error())
			}
		}
	}

	if err := h.writeTLSDataFromSecret(); err != nil {
		log.Errorf("%s", err.Error())
	}
}
