package counter

import (
	"testing"

	"github.com/splitio/go-toolkit/v5/logging"
)

func TestPeriodicCounter(t *testing.T) {

	counter := NewPeriodicCounter(&Config{
		Name:                     "Test",
		CounterType:              0,
		TaskFunc:                 func(l logging.LoggerInterface, c ApplicationCounterInterface) error { return nil },
		Period:                   2,
		MaxErrorsAllowedInPeriod: 2,
		Severity:                 0,
		MonitorType:              Periodic,
	}, logging.NewLogger(nil))

	counter.Start()

	res := counter.IsHealthy()
	if !res.Healthy {
		t.Errorf("Healthy should be true")
	}

	counter.NotifyEvent()
	res = counter.IsHealthy()
	if !res.Healthy {
		t.Errorf("Healthy should be true")
	}

	counter.Reset(0)
	res = counter.IsHealthy()
	if !res.Healthy {
		t.Errorf("Healthy should be true")
	}

	if counter.errorCount != 0 {
		t.Errorf("Errors should be 0")
	}

	counter.NotifyEvent()
	counter.NotifyEvent()
	res = counter.IsHealthy()
	if res.Healthy {
		t.Errorf("Healthy should be false")
	}

	if counter.errorCount != 2 {
		t.Errorf("Errors should be 2")
	}

	counter.Stop()
}
