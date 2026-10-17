package core

import (
	"fmt"
	"time"
)

// defaultMeterFailsafe is the time the site holds the charger state while the grid meter is unavailable
const defaultMeterFailsafe = 10 * time.Minute

// MeterFailure is the degradation level applied to loadpoints while the site's meters are unavailable
type MeterFailure int

const (
	// MeterOK indicates that all meters were read successfully
	MeterOK MeterFailure = iota
	// MeterHold keeps the charger state unchanged until the meters recover or the failsafe kicks in
	MeterHold
	// MeterFailsafe charges at least with minimum current like always charge
	MeterFailsafe
)

// MeterFailsafeAction is the loadpoint behavior once the meter failsafe timeout has elapsed
type MeterFailsafeAction string

const (
	// MeterFailsafeAuto pauses heating and charges everything else with at least min current
	MeterFailsafeAuto MeterFailsafeAction = ""
	// MeterFailsafeCharge charges with at least min current
	MeterFailsafeCharge MeterFailsafeAction = "charge"
	// MeterFailsafePause disables the charger
	MeterFailsafePause MeterFailsafeAction = "pause"
	// MeterFailsafeHold keeps the charger state unchanged
	MeterFailsafeHold MeterFailsafeAction = "hold"
)

// validate checks the configured action
func (a MeterFailsafeAction) validate() error {
	switch a {
	case MeterFailsafeAuto, MeterFailsafeCharge, MeterFailsafePause, MeterFailsafeHold:
		return nil
	default:
		return fmt.Errorf("invalid onMeterFailure: %q (charge, pause, hold)", string(a))
	}
}

func (m MeterFailure) String() string {
	switch m {
	case MeterHold:
		return "hold"
	case MeterFailsafe:
		return "failsafe"
	default:
		return "ok"
	}
}

// trackMeterFailure records the meter read result and returns the degradation level to apply
func (site *Site) trackMeterFailure(failed bool) MeterFailure {
	res := MeterOK

	if failed {
		if site.meterFailedSince.IsZero() {
			site.meterFailedSince = time.Now()
		}

		res = MeterHold
		if site.MeterFailsafe > 0 && time.Since(site.meterFailedSince) >= site.MeterFailsafe {
			res = MeterFailsafe
		}
	} else if !site.meterFailedSince.IsZero() {
		site.log.INFO.Printf("meters recovered after %v", time.Since(site.meterFailedSince).Round(time.Second))
		site.meterFailedSince = time.Time{}
	}

	if res != site.meterFailure {
		site.log.WARN.Printf("meter failure state: %s -> %s", site.meterFailure, res)
		site.meterFailure = res
	}

	return res
}
