package charger

// LICENSE

// Copyright (c) evcc.io (andig, naltatis, premultiply)

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/logstash"
	"github.com/evcc-io/evcc/util/modbus"
	jww "github.com/spf13/jwalterweatherman"
	"github.com/volkszaehler/mbmd/meters/rs485"
)

// eSolutions eProWallbox charger implementation
type EProWallbox struct {
	conn       *modbus.Connection
	cache      *eproCache
	log        *util.Logger
	lastStatus uint16
}

const (
	eproRegStatus         = 40101 // IEC 61851 Status, 1 register, UINT16
	eproRegEnable         = 40406 // On/Off state, 1 registers, UINT16
	eproRegCurrentLimit   = 40407 // in mA
	eproRegResetWatchdog  = 40502
	eproRegVoltages       = 40604 // L1 voltage in V, 2 registers, Float32 (followed by L2, L3)
	eproRegCurrents       = 40620 // L1 current in A, 2 registers, Float32 (followed by L2, L3)
	eproRegPowers         = 40636 // L1 power in W, 2 registers, Float32 (followed by L2, L3)
	eproRegActiveEnergies = 40658 // L1 energy in Wh, 2 registers, Float32 (followed by L2, L3)

	eproRegBlockStatus = 40100 // 40100..40108: error, status, ocpp status, limits, derating
	eproRegBlockMeter  = 40600 // 40600..40663: charge time, voltages, currents, powers, energies

	// eproCacheTTL bounds the age of status and meter blocks, which only deduplicates reads within one control cycle
	eproCacheTTL = 2 * time.Second

	// eproConfigCacheTTL bounds the age of the enable/limit block, which is kept current by writes.
	// The refresh still detects state changes made by the device itself.
	eproConfigCacheTTL = 60 * time.Second
)

func init() {
	registry.AddCtx("eprowallbox", NewEProWallboxFromConfig)
}

// NewEProWallboxFromConfig creates a eProWallbox charger from generic config
func NewEProWallboxFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := modbus.Settings{
		ID: 1,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewEProWallbox(ctx, cc)
}

// NewEProWallbox creates eProWallbox charger
func NewEProWallbox(ctx context.Context, settings modbus.Settings) (api.Charger, error) {
	conn, err := settings.Connection(ctx)
	if err != nil {
		return nil, err
	}

	log := util.NewLogger("eprowallbox")
	conn.Logger(log.TRACE)

	wb := &EProWallbox{
		conn: conn,
		log:  log,
		// status, enable/limit/watchdog and meter registers are each fetched with a single request
		cache: newEProCache(conn, log, eproCacheTTL,
			[2]uint16{eproRegBlockStatus, 9},
			[2]uint16{eproRegEnable, 8},
			[2]uint16{eproRegBlockMeter, 64},
		),
	}
	wb.cache.onFetch = wb.logBlock
	wb.cache.setTTL(eproRegEnable, eproConfigCacheTTL)

	go wb.heartbeat(ctx)

	return wb, nil
}

func (wb *EProWallbox) heartbeat(ctx context.Context) {
	for tick := time.Tick(10 * time.Second); ; {
		select {
		case <-tick:
		case <-ctx.Done():
			return
		}

		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, 0x5555)
		if _, err := wb.conn.WriteMultipleRegisters(eproRegResetWatchdog, 1, b); err != nil {
			wb.log.ERROR.Println("heartbeat:", err)
		}
	}
}

// Status implements the api.Charger interface
func (wb *EProWallbox) Status() (api.ChargeStatus, error) {
	b, err := wb.cache.read(eproRegStatus, 1)
	if err != nil {
		return api.StatusNone, err
	}

	s := binary.BigEndian.Uint16(b)

	if s != wb.lastStatus {
		wb.log.DEBUG.Printf("status transition: %s -> %s",
			decoderGeneralStatus[wb.lastStatus], decoderGeneralStatus[s])
		wb.lastStatus = s
	}

	switch s {
	case 0, 1: // A1, A2
		return api.StatusA, nil
	case 2, 3, 4, 6: // B1, B2, C1, D1
		return api.StatusB, nil
	case 5, 7: // C2, D2
		return api.StatusC, nil
	default:
		return api.StatusNone, fmt.Errorf("invalid status: %d", s)
	}
}

// Enabled implements the api.Charger interface
func (wb *EProWallbox) Enabled() (bool, error) {
	b, err := wb.cache.read(eproRegEnable, 1)
	if err != nil {
		return false, err
	}

	return binary.BigEndian.Uint16(b) != 0, nil
}

// Enable implements the api.Charger interface
func (wb *EProWallbox) Enable(enable bool) error {
	b := make([]byte, 2)
	if enable {
		binary.BigEndian.PutUint16(b, 1)
	}

	return wb.write(eproRegEnable, 1, b)
}

// MaxCurrent implements the api.Charger interface
func (wb *EProWallbox) MaxCurrent(current int64) error {
	return wb.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*EProWallbox)(nil)

// MaxCurrent implements the api.ChargerEx interface
func (wb *EProWallbox) MaxCurrentMillis(current float64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %.5g", current)
	}

	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(current*1e3))

	return wb.write(eproRegCurrentLimit, 2, b)
}

// write writes registers and keeps the cache in sync
func (wb *EProWallbox) write(address, quantity uint16, b []byte) error {
	if _, err := wb.conn.WriteMultipleRegisters(address, quantity, b); err != nil {
		wb.cache.invalidate()
		return err
	}

	wb.cache.write(address, b)
	return nil
}

// getPhaseValues returns 3 sequential register values
func (wb *EProWallbox) getPhaseValues(reg uint16, divider float64) (float64, float64, float64, error) {
	b, err := wb.cache.read(reg, 6)
	if err != nil {
		return 0, 0, 0, err
	}

	var res [3]float64
	for i := range res {
		res[i] = rs485.RTUIeee754ToFloat64(b[4*i:]) / divider
	}

	return res[0], res[1], res[2], nil
}

var _ api.Meter = (*EProWallbox)(nil)

// CurrentPower implements the api.Meter interface
func (wb *EProWallbox) CurrentPower() (float64, error) {
	l1, l2, l3, err := wb.getPhaseValues(eproRegPowers, 1)
	return l1 + l2 + l3, err
}

var _ api.MeterEnergy = (*EProWallbox)(nil)

// TotalEnergy implements the api.MeterEnergy interface
func (wb *EProWallbox) TotalEnergy() (float64, error) {
	l1, l2, l3, err := wb.getPhaseValues(eproRegActiveEnergies, 1000)
	return -(l1 + l2 + l3), err
}

var _ api.PhaseCurrents = (*EProWallbox)(nil)

// Currents implements the api.PhaseCurrents interface
func (wb *EProWallbox) Currents() (float64, float64, float64, error) {
	return wb.getPhaseValues(eproRegCurrents, 1)
}

var _ api.PhaseVoltages = (*EProWallbox)(nil)

// Voltages implements the api.PhaseVoltages interface
func (wb *EProWallbox) Voltages() (float64, float64, float64, error) {
	return wb.getPhaseValues(eproRegVoltages, 1)
}

var _ api.Resurrector = (*EProWallbox)(nil)

// WakeUp implements the api.Resurrector interface
func (wb *EProWallbox) WakeUp() error {
	wb.log.WARN.Println("WakeUp() triggered - vehicle in SuspendedEV, performing CP interrupt")
	if err := wb.Enable(false); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)

	// refill the cache to log the state during the CP interrupt
	_, _ = wb.cache.read(eproRegStatus, 1)

	err := wb.Enable(true)

	// temporary diagnostic: dump log buffer to file 90s after wakeup
	go wb.dumpDiagnosticLog()

	return err
}

// dumpDiagnosticLog waits 90s then writes the ring buffer to a timestamped file.
// Temporary diagnostic - remove once SuspendedEV behavior is confirmed resolved.
func (wb *EProWallbox) dumpDiagnosticLog() {
	time.Sleep(90 * time.Second)

	lines := logstash.All(nil, jww.LevelTrace, 0)
	if len(lines) == 0 {
		return
	}

	filename := fmt.Sprintf("eprowallbox-wakeup-%s.log", time.Now().Format("20060102-150405"))
	if err := os.WriteFile(filename, []byte(strings.Join(lines, "")), 0o644); err != nil {
		wb.log.ERROR.Printf("diagnostic log dump failed: %v", err)
	} else {
		wb.log.WARN.Printf("diagnostic log saved: %s", filename)
	}
}

// use description from modbus communication map pdf from Free2Move
var decoderOcppStatus = map[uint16]string{
	0: "Available (A)", 1: "Preparing (B)", 2: "Charging (C)",
	3: "SuspendedEV (D)", 4: "SuspendedEVSE (E)", 5: "Finishing (F)",
	6: "Reserved (G)", 7: "Unavailable (H)", 8: "Faulted (I)",
}

var decoderGeneralStatus = map[uint16]string{
	0: "A1", 1: "A2", 2: "B1", 3: "B2", 4: "C1", 5: "C2", 6: "D1", 7: "D2", 8: "E", 9: "F",
}

// logBlock logs the decoded registers of a freshly fetched block
func (wb *EProWallbox) logBlock(start uint16, b []byte) {
	u16 := func(address uint16) uint16 {
		return binary.BigEndian.Uint16(b[2*(address-start):])
	}
	u32 := func(address uint16) uint32 {
		return binary.BigEndian.Uint32(b[2*(address-start):])
	}
	decode := func(m map[uint16]string, value uint16) string {
		if decoded, ok := m[value]; ok {
			return decoded
		}
		return fmt.Sprintf("Unknown (%d)", value)
	}

	switch start {
	case eproRegBlockStatus:
		wb.log.DEBUG.Printf("OCPP: %s | Status: %s | Error: %d | UserLimit: %d mA | UB: %d | DPM: %d | TempDerate: %d",
			decode(decoderOcppStatus, u16(40102)), decode(decoderGeneralStatus, u16(40101)), u16(40100),
			u32(40103), u16(40106), u16(40107), u16(40108))

	case eproRegEnable:
		wb.log.DEBUG.Printf("On/Off: %d | Limit: %d mA | WD: %d/%d s",
			u16(eproRegEnable), u32(eproRegCurrentLimit), u16(40412), u16(40413))

	case eproRegBlockMeter:
		wb.log.DEBUG.Printf("Charge Time: %d s | I: %.1f/%.1f/%.1f A | V: %.1f/%.1f/%.1f V",
			u32(40600),
			rs485.RTUIeee754ToFloat64(b[2*(eproRegCurrents-start):]),
			rs485.RTUIeee754ToFloat64(b[2*(eproRegCurrents+2-start):]),
			rs485.RTUIeee754ToFloat64(b[2*(eproRegCurrents+4-start):]),
			rs485.RTUIeee754ToFloat64(b[2*(eproRegVoltages-start):]),
			rs485.RTUIeee754ToFloat64(b[2*(eproRegVoltages+2-start):]),
			rs485.RTUIeee754ToFloat64(b[2*(eproRegVoltages+4-start):]))
	}
}
