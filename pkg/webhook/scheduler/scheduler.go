package scheduler

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// certRenewalHandler is the renewal surface the scheduling loop drives.
type certRenewalHandler interface {
	GetCertExpireDate() (expireDate time.Time, err error)
	Run(certRenewalPeriod int64) error
}

// serverHandler is the admission server surface the loop restarts after
// a successful renewal.
type serverHandler interface {
	Stop() error
	Run()
}

// timerFunc returns a channel which receives once after d has elapsed
// and a stop func which disarms the timer. the production implementation
// is time.NewTimer; the loop's regression substitutes a manually fired
// channel, so many renewals run without real waiting.
type timerFunc func(d time.Duration) (fire <-chan time.Time, stop func())

func systemTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)

	return t.C, func() { t.Stop() }
}

// StartCertRenewalScheduler runs the one renewal loop of the process on
// the given context (F14): the previous design reassigned a global
// ticker and recursively started another scheduler on every tick, so
// each renewal leaked its predecessor's goroutine - blocked forever on
// a stopped ticker whose quit channel nobody closes - and short
// certificate lifetimes accumulated waiters quickly. the loop now owns
// one local timer per wait, exits on the context's cancellation and is
// stopped by the same cancel which begins the process shutdown, so no
// tick can restart the admission server under the shutdown drain.
func StartCertRenewalScheduler(ctx context.Context, cHandler certRenewalHandler, sHandler serverHandler, certRenewalPeriod int64) {
	go runRenewalLoop(ctx, cHandler, sHandler, certRenewalPeriod, systemTimer)
}

func runRenewalLoop(ctx context.Context, cHandler certRenewalHandler, sHandler serverHandler, certRenewalPeriod int64, newTimer timerFunc) {
	for {
		delay := time.Duration(nextRenewalDelay(cHandler, certRenewalPeriod)) * time.Minute
		fire, stop := newTimer(delay)
		select {
		case <-fire:
			log.Infof("certRenewalPeriod is reached, renewing certificate and secret")
			if err := cHandler.Run(certRenewalPeriod); err != nil {
				// the renewal produced no usable pair (F13): the server
				// keeps serving its current credentials, and the next
				// tick retries - no server restart on a failed renewal
				log.Errorf("(webhook.scheduler) the certificate renewal failed, keeping the serving credentials: %s", err.Error())
			} else {
				if err := sHandler.Stop(); err != nil {
					log.Warnf("(webhook.scheduler) the drained server did not close cleanly: %s", err.Error())
				}
				// the shutdown may have landed while the loop was inside
				// the drain: the re-listen is the one action which must
				// never happen after the cancellation, so the loop
				// re-checks the context between the drain and the
				// restart instead of trusting the select it left
				if ctx.Err() != nil {
					log.Infof("(webhook.scheduler) the renewal scheduling stopped: %s", ctx.Err().Error())

					return
				}
				go sHandler.Run()
			}
		case <-ctx.Done():
			// the pending timer is disarmed and the loop returns: the
			// one goroutine this loop owns exits with the process
			// context, and the re-listen guard above keeps the server
			// down even when the cancellation lands mid-drain
			stop()
			log.Infof("(webhook.scheduler) the renewal scheduling stopped: %s", ctx.Err().Error())

			return
		}
	}
}

// nextRenewalDelay returns the minutes until the next renewal is due.
func nextRenewalDelay(cHandler certRenewalHandler, certRenewalPeriod int64) int64 {
	// the expiry of an unusable pair must not kill the process (F13):
	// the persisted secret survives restarts, so a panic here was a
	// permanent restart loop which no restart could heal. the repair
	// runs on the next tick, so re-check at the minimum interval
	// instead
	expireDate, err := cHandler.GetCertExpireDate()
	if err != nil {
		log.Warnf("(webhook.scheduler) cannot determine the certificate expiry, re-checking at the minimum interval: %s", err.Error())

		return 1
	}

	return renewalDelayMinutes(expireDate, time.Now().UTC(), certRenewalPeriod)
}

// renewalDelayMinutes returns the minutes until the renewal is due for
// a certificate expiring at expireDate, relative to now.
func renewalDelayMinutes(expireDate time.Time, now time.Time, certRenewalPeriod int64) int64 {
	difference := expireDate.Sub(now)
	// we always need 1 min extra because if the expire time is 0 the cert is still valid
	sTime := int64(difference.Minutes()) - certRenewalPeriod + 1
	if sTime < 1 {
		// the timer cannot be 0 or negative
		sTime = 1
	}

	return sTime
}
