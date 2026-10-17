package core

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
)

// chargerHasFeature checks availability of charger feature
// meterFailsafeAction returns the effective failsafe action. Heating only serves surplus use,
// there is no purpose in running it without surplus data.
func (lp *Loadpoint) meterFailsafeAction() MeterFailsafeAction {
	if lp.OnMeterFailure != MeterFailsafeAuto {
		return lp.OnMeterFailure
	}
	if lp.chargerHasFeature(api.Heating) {
		return MeterFailsafePause
	}
	return MeterFailsafeCharge
}

func (lp *Loadpoint) chargerHasFeature(f api.Feature) bool {
	return hasFeature(lp.charger, f)
}

// publishChargerFeature publishes availability of charger features
func (lp *Loadpoint) publishChargerFeature(f api.Feature) {
	lp.publish(keys.ChargerFeature+f.String(), lp.chargerHasFeature(f))
}
