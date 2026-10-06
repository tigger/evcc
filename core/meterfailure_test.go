package core

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
)

func TestTrackMeterFailure(t *testing.T) {
	site := &Site{log: util.NewLogger("foo"), MeterFailsafe: time.Minute}

	assert.Equal(t, MeterOK, site.trackMeterFailure(false))
	assert.Equal(t, MeterHold, site.trackMeterFailure(true))
	assert.Equal(t, MeterHold, site.trackMeterFailure(true))

	site.meterFailedSince = time.Now().Add(-time.Minute)
	assert.Equal(t, MeterFailsafe, site.trackMeterFailure(true))

	assert.Equal(t, MeterOK, site.trackMeterFailure(false))
	assert.True(t, site.meterFailedSince.IsZero())
}

func TestTrackMeterFailureNoFailsafe(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}

	site.trackMeterFailure(true)
	site.meterFailedSince = time.Now().Add(-24 * time.Hour)

	assert.Equal(t, MeterHold, site.trackMeterFailure(true), "hold forever without failsafe timeout")
}

func TestPVMaxCurrentMeterFailsafe(t *testing.T) {
	tc := []struct {
		name    string
		failure MeterFailure
		enabled bool
		current float64
	}{
		{"ok, disabled", MeterOK, false, 0},
		{"failsafe, disabled", MeterFailsafe, false, minA},
		{"failsafe, enabled", MeterFailsafe, true, minA},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			Voltage = 230

			lp := &Loadpoint{
				log:            util.NewLogger("foo"),
				clock:          clock.NewMock(),
				charger:        &api.MockCharger{},
				minCurrent:     minA,
				maxCurrent:     maxA,
				phases:         3,
				measuredPhases: 3,
				solarShare:     1,
				status:         api.StatusC,
				enabled:        tc.enabled,
				Enable:         loadpoint.ThresholdConfig{Delay: time.Minute},
				Disable:        loadpoint.ThresholdConfig{Delay: time.Minute},
			}
			lp.SetMeterFailure(tc.failure)

			assert.Equal(t, tc.current, lp.pvMaxCurrent(0, 0, false, false))
		})
	}
}
