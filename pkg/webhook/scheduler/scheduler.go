package scheduler

import (
	"time"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/webhook/config"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/webhook/service"
	log "github.com/sirupsen/logrus"
)

var ticker *time.Ticker

func StartCertRenewalScheduler(cHandler *config.Handler, sHandler *service.Handler, certRenewalPeriod int64) {
	var sTime int64

	// the expiry of an unusable pair must not kill the process (F13):
	// the persisted secret survives restarts, so the previous panic here
	// was a permanent restart loop which no restart could heal. the
	// repair runs on the next tick, so re-check at the minimum interval
	// instead
	expireDate, err := cHandler.GetCertExpireDate()
	if err != nil {
		log.Warnf("(webhook.scheduler) cannot determine the certificate expiry, re-checking at the minimum interval: %s", err.Error())
		sTime = 1
	} else {
		currentDate := time.Now().UTC()
		difference := expireDate.Sub(currentDate)
		// we always need 1 min extra because if the expire time is 0 the cert is still valid
		sTime = int64(difference.Minutes()) - certRenewalPeriod + 1
		if sTime < 1 {
			// the ticker cannot be 0 or negative
			sTime = 1
		}
	}

	ticker = time.NewTicker(time.Duration(sTime) * time.Minute)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				log.Infof("certRenewalPeriod is reached, renewing certificate and secret")
				if err := cHandler.Run(certRenewalPeriod); err != nil {
					// the renewal produced no usable pair (F13): the server
					// keeps serving its current credentials, and the next
					// tick retries - no server restart on a failed renewal
					log.Errorf("(webhook.scheduler) the certificate renewal failed, keeping the serving credentials: %s", err.Error())
				} else {
					sHandler.Stop()
					go sHandler.Run()
				}
				ticker.Stop()
				StartCertRenewalScheduler(cHandler, sHandler, certRenewalPeriod)
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
}
