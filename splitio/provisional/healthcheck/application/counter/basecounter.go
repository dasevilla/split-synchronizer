package counter

import (
	"sync"
	"time"

	hcCommon "github.com/splitio/go-split-commons/v4/healthcheck/application"
	"github.com/splitio/go-toolkit/v5/logging"
)

// ApplicationCounterImp description
type ApplicationCounterImp struct {
	name        string
	counterType int
	lastHit     *time.Time
	healthy     bool
	running     bool
	period      int
	severity    int
	errorCount  int
	lock        sync.RWMutex
	logger      logging.LoggerInterface
}

func (c *ApplicationCounterImp) updateLastHit() {
	now := time.Now()
	c.lastHit = &now
}

// GetType return counter type
func (c *ApplicationCounterImp) GetType() int {
	return c.counterType
}

// IsHealthy return the counter health
func (c *ApplicationCounterImp) IsHealthy() hcCommon.HealthyResult {
	return hcCommon.HealthyResult{
		Name:       c.name,
		Healthy:    c.healthy,
		Severity:   c.severity,
		LastHit:    c.lastHit,
		ErrorCount: c.errorCount,
	}
}

// NewApplicationCounterImp create an application counter
func NewApplicationCounterImp(
	name string,
	counterType int,
	period int,
	severity int,
	logger logging.LoggerInterface,
) *ApplicationCounterImp {
	return &ApplicationCounterImp{
		name:        name,
		lock:        sync.RWMutex{},
		logger:      logger,
		healthy:     true,
		running:     false,
		counterType: counterType,
		period:      period,
		severity:    severity,
	}
}
