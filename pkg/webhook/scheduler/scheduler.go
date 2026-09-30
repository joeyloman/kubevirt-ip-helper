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

	expireDate, err := cHandler.GetCertExpireDate()
	if err != nil {
		log.Panicf("%s", err.Error())
	}

	currentDate := time.Now().UTC()
	difference := expireDate.Sub(currentDate)
	// we always need 1 min extra because if the expire time is 0 the cert is still valid
	sTime = int64(difference.Minutes()) - certRenewalPeriod + 1
	if sTime < 1 {
		// the ticker cannot be 0 or negative
		sTime = 1
	}

	ticker = time.NewTicker(time.Duration(sTime) * time.Minute)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				log.Infof("certRenewalPeriod is reached, renewing certificate and secret")
				cHandler.Run(certRenewalPeriod)
				sHandler.Stop()
				go sHandler.Run()
				ticker.Stop()
				StartCertRenewalScheduler(cHandler, sHandler, certRenewalPeriod)
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
}
