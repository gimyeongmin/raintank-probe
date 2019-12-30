package scheduler

import (
	"sync"
	"time"

	"github.com/grafana/metrictank/stats"
	"github.com/raintank/raintank-probe/checks"
	log "github.com/sirupsen/logrus"
)

var (
	schedulerHealth = stats.NewGauge32("scheduler.healthy")
)

// Ping scheduler.HealthHosts to determin if this probe is healthy and should
// execute checks.  If all of the HealthHosts are experiencing issues, then
// there is likely something wrong with this probe so it should stop executing
// checks until things recover.
//
func (s *Scheduler) CheckHealth() {
	chks := make([]*checks.RaintankProbePing, len(s.HealthHosts))
	for i, host := range s.HealthHosts {
		settings := make(map[string]interface{})
		settings["timeout"] = 2.0
		settings["hostname"] = host
		chk, err := checks.NewRaintankPingProbe(settings)
		if err != nil {
			log.Fatalf("unable to create health check. %s", err)
		}
		chks[i] = chk
	}
	schedulerHealth.Set(0)
	lastState := 1

	ticker := time.NewTicker(time.Second * 5)
	var wg sync.WaitGroup
	for range ticker.C {
		resultsCh := make(chan int, len(chks))
		for i := range chks {
			check := chks[i]
			wg.Add(1)
			go func(ch chan int, chk *checks.RaintankProbePing) {
				defer wg.Done()
				results, err := chk.Run()
				if err != nil {
					log.Warningf("Health check to %s failed. %s", chk.Hostname, err)
					ch <- 3
					return
				}
				if results.ErrorMsg() != "" {
					log.Warningf("Health check to %s failed. %s", chk.Hostname, results.ErrorMsg())
					ch <- 1
					return
				}
				log.Debugf("Health check completed for %s", chk.Hostname)
				ch <- 0
			}(resultsCh, check)
		}
		wg.Wait()
		close(resultsCh)
		score := 0
		for r := range resultsCh {
			if r == 3 {
				// fatal error, trying to run the check.
				score = len(chks)
			} else {
				score += r
			}
		}
		newState := 0
		// if more the 50% of healthHosts are down, then we consider ourselves down.
		if float64(score) > float64(len(chks)/2.0) {
			newState = 1
		}

		if newState != lastState {
			if newState == 1 {
				// we are now unhealthy.
				schedulerHealth.Set(0)
				s.Lock()
				log.Warning("This probe is in an unhealthy state. Stopping execution of checks.")
				s.Healthy = false
				for _, instance := range s.Checks {
					instance.Stop()
				}
				s.Unlock()
			} else {
				//we are now healthy.
				schedulerHealth.Set(1)
				s.Lock()
				log.Warning("This probe is now healthy again. Resuming execution of checks.")
				s.Healthy = true
				for _, instance := range s.Checks {
					log.Debugf("resuming %s check for %s", instance.Check.Type, instance.Check.Slug)
					instance.Run()
				}
				s.Unlock()
			}
			lastState = newState
		}
	}
}
