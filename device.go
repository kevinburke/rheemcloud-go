package rheemcloud

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DeviceType identifies the kind of equipment Rheem returns.
type DeviceType int

const (
	DeviceUnknown DeviceType = iota
	DeviceWaterHeater
	DeviceThermostat
)

func (d DeviceType) String() string {
	switch d {
	case DeviceWaterHeater:
		return "water_heater"
	case DeviceThermostat:
		return "thermostat"
	default:
		return "unknown"
	}
}

func deviceTypeFromString(s string) DeviceType {
	switch s {
	case "WH":
		return DeviceWaterHeater
	case "HVAC":
		return DeviceThermostat
	default:
		return DeviceUnknown
	}
}

// Mode is the operating mode of a water heater. Values match
// pyeconet.WaterHeaterOperationMode so labels are stable across
// libraries.
type Mode int

const (
	ModeUnknown      Mode = 99
	ModeOff          Mode = 1
	ModeElectric     Mode = 2 // matches "Electric" / "Electric Mode" / "Performance"
	ModeEnergySaving Mode = 3 // "Energy Saver" / "Energy Saving" / "Eco"
	ModeHeatPump     Mode = 4 // "Heat Pump" / "Heat Pump Only"
	ModeHighDemand   Mode = 5
	ModeGas          Mode = 6
	ModePerformance  Mode = 8
	ModeVacation     Mode = 9
)

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeElectric:
		return "electric"
	case ModeEnergySaving:
		return "energy-saving"
	case ModeHeatPump:
		return "heat-pump"
	case ModeHighDemand:
		return "high-demand"
	case ModeGas:
		return "gas"
	case ModePerformance:
		return "performance"
	case ModeVacation:
		return "vacation"
	default:
		return "unknown"
	}
}

// modeFromEnumText maps a Rheem mode label (as returned in
// @MODE.constraints.enumText) to a Mode. The label normalization
// matches pyeconet's WaterHeaterOperationMode.by_string.
func modeFromEnumText(s string) Mode {
	cleaned := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimRight(s, " "), " ", "_"), "/", "_"))
	switch cleaned {
	case "OFF":
		return ModeOff
	case "ELECTRIC", "ELECTRIC_MODE":
		return ModeElectric
	case "ENERGY_SAVING", "ENERGY_SAVER":
		return ModeEnergySaving
	case "HEAT_PUMP", "HEAT_PUMP_ONLY":
		return ModeHeatPump
	case "HIGH_DEMAND":
		return ModeHighDemand
	case "GAS":
		return ModeGas
	case "PERFORMANCE":
		return ModePerformance
	case "VACATION":
		return ModeVacation
	default:
		return ModeUnknown
	}
}

// Device represents a single piece of Rheem equipment associated with
// the logged-in account. Currently water heaters are the only fully-
// supported type; thermostat support is stubbed.
type Device struct {
	api *Client

	mu   sync.Mutex
	info map[string]json.RawMessage // current view of the JSON document

	// LastUpdated is the time we last applied an incoming MQTT update
	// or refresh. Zero before the first event.
	LastUpdated time.Time
}

// newDevice wraps a freshly-decoded equipment block.
func newDevice(api *Client, info map[string]json.RawMessage) *Device {
	return &Device{api: api, info: info}
}

// Raw returns the device's full raw JSON document — every field the
// REST + MQTT protocol has surfaced so far, including the "@..."
// datapoints (mode, setpoint, alerts, etc.) and the bare-string
// metadata fields (serial_number, device_name, device_type, etc.).
// The result is a snapshot; subsequent MQTT updates won't affect it.
func (d *Device) Raw() map[string]json.RawMessage {
	return d.snapshotInfo()
}

// snapshotInfo returns a shallow copy of the current info map under
// lock. Callers may then run accessors on the result without holding
// the device lock.
func (d *Device) snapshotInfo() map[string]json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]json.RawMessage, len(d.info))
	maps.Copy(out, d.info)
	return out
}

// applyUpdate merges an incoming MQTT update payload into the device's
// info map. The keys prefixed with "@" carry datapoint deltas; if the
// existing value is an object with a "value" sub-key, only the inner
// value is replaced (preserving constraints/status). This mirrors
// pyeconet's Equipment.update_equipment_info.
func (d *Device) applyUpdate(update map[string]json.RawMessage) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for k, v := range update {
		if !strings.HasPrefix(k, "@") {
			continue
		}
		existing, ok := d.info[k]
		if !ok {
			d.info[k] = v
			changed = true
			continue
		}
		// Distinguish object vs scalar by peeking at the first byte.
		if isJSONObject(existing) {
			merged, ok := mergeIntoObject(existing, v)
			if ok {
				d.info[k] = merged
				changed = true
				continue
			}
		}
		d.info[k] = v
		changed = true
	}
	if changed {
		d.LastUpdated = time.Now()
	}
	return changed
}

func isJSONObject(b json.RawMessage) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// mergeIntoObject takes an existing JSON object and an update RawMessage.
// If the update is itself an object, its keys are merged into the
// existing object. If the update is a scalar AND the existing object has
// a "value" key, the scalar replaces the "value" field. Returns the new
// raw bytes and true on success.
func mergeIntoObject(existing, update json.RawMessage) (json.RawMessage, bool) {
	var existingObj map[string]json.RawMessage
	if err := json.Unmarshal(existing, &existingObj); err != nil {
		return nil, false
	}
	if isJSONObject(update) {
		var updateObj map[string]json.RawMessage
		if err := json.Unmarshal(update, &updateObj); err != nil {
			return nil, false
		}
		maps.Copy(existingObj, updateObj)
	} else {
		if _, hasValue := existingObj["value"]; hasValue {
			existingObj["value"] = update
		} else {
			return nil, false
		}
	}
	b, err := json.Marshal(existingObj)
	if err != nil {
		return nil, false
	}
	return b, true
}

// --- Accessors -----------------------------------------------------------

// DeviceID is the opaque device name Rheem uses on MQTT topics. Stable
// for the life of the unit.
func (d *Device) DeviceID() string {
	info := d.snapshotInfo()
	var s string
	if v, ok := info["device_name"]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return s
}

// SerialNumber returns the unit's serial.
func (d *Device) SerialNumber() string {
	info := d.snapshotInfo()
	var s string
	if v, ok := info["serial_number"]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return s
}

// Type returns the device's category.
func (d *Device) Type() DeviceType {
	info := d.snapshotInfo()
	var s string
	if v, ok := info["device_type"]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return deviceTypeFromString(s)
}

// FriendlyName returns @NAME.value if present, else the device ID.
func (d *Device) FriendlyName() string {
	info := d.snapshotInfo()
	if v, ok := info["@NAME"]; ok {
		if obj := decodeDatapoint(v); obj != nil {
			var s string
			if err := json.Unmarshal(obj.Value, &s); err == nil && s != "" {
				return s
			}
		}
	}
	return d.DeviceID()
}

// GenericType returns the @TYPE string (e.g. "heatpumpWaterHeater",
// "gasWaterHeater", "tanklessWaterHeater").
func (d *Device) GenericType() string {
	info := d.snapshotInfo()
	if v, ok := info["@TYPE"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s
		}
	}
	return ""
}

// Connected reports whether the unit is talking to Rheem's cloud.
// Returns true if @CONNECTED is absent (older units don't include it
// and are assumed connected).
func (d *Device) Connected() bool {
	info := d.snapshotInfo()
	v, ok := info["@CONNECTED"]
	if !ok {
		return true
	}
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}

// Setpoint returns the configured water temperature setpoint, or 0 if
// the unit doesn't expose one.
func (d *Device) Setpoint() int {
	info := d.snapshotInfo()
	v, ok := info["@SETPOINT"]
	if !ok {
		return 0
	}
	obj := decodeDatapoint(v)
	if obj == nil {
		return 0
	}
	var n float64
	_ = json.Unmarshal(obj.Value, &n)
	return int(n)
}

// SetpointLimits returns the min and max setpoint values from the
// device's reported constraints. Returns 0,0 if no constraints are
// reported.
func (d *Device) SetpointLimits() (min, max int) {
	info := d.snapshotInfo()
	v, ok := info["@SETPOINT"]
	if !ok {
		return 0, 0
	}
	obj := decodeDatapoint(v)
	if obj == nil {
		return 0, 0
	}
	return int(obj.Constraints.LowerLimit), int(obj.Constraints.UpperLimit)
}

// AlertCount returns the @ALERTCOUNT field (number of active alerts).
func (d *Device) AlertCount() int {
	info := d.snapshotInfo()
	v, ok := info["@ALERTCOUNT"]
	if !ok {
		return 0
	}
	var n float64
	if err := json.Unmarshal(v, &n); err == nil {
		return int(n)
	}
	// Some accounts return the count as a string.
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if i, err := strconv.Atoi(s); err == nil {
			return i
		}
	}
	return 0
}

// WiFiSignal returns the @SIGNAL value (dB) or 0 if not yet received.
// The signal field only arrives via MQTT updates, not the initial REST
// device list — it'll be zero until the first MQTT message lands.
func (d *Device) WiFiSignal() int {
	info := d.snapshotInfo()
	v, ok := info["@SIGNAL"]
	if !ok {
		return 0
	}
	var n float64
	if err := json.Unmarshal(v, &n); err == nil {
		return int(n)
	}
	if obj := decodeDatapoint(v); obj != nil {
		_ = json.Unmarshal(obj.Value, &n)
		return int(n)
	}
	return 0
}

// SupportsAway reports whether the unit accepts away-mode commands.
func (d *Device) SupportsAway() bool {
	info := d.snapshotInfo()
	v, ok := info["@AWAYCONFIG"]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}

// Away reports whether away mode is currently active.
func (d *Device) Away() bool {
	info := d.snapshotInfo()
	v, ok := info["@AWAY"]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}

// Running reports whether the heater is actively heating right now,
// based on the @RUNNING string being non-empty.
func (d *Device) Running() bool {
	return d.RunningState() != ""
}

// RunningState returns the raw @RUNNING string (often a status label
// like "Heat Pump On" or "").
func (d *Device) RunningState() string {
	info := d.snapshotInfo()
	v, ok := info["@RUNNING"]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// HotWaterAvailability returns the unit's "how much hot water is left"
// estimate, mapped to 0/33/66/100 like pyeconet does. Returns -1 if
// the unit doesn't report this field.
func (d *Device) HotWaterAvailability() int {
	info := d.snapshotInfo()
	v, ok := info["@HOTWATER"]
	if !ok {
		return -1
	}
	var icon string
	if err := json.Unmarshal(v, &icon); err != nil {
		return -1
	}
	switch {
	case strings.Contains(icon, "ic_tank_hundread_percent"):
		return 100
	case strings.Contains(icon, "ic_tank_fourty_percent"):
		return 66
	case strings.Contains(icon, "ic_tank_ten_percent"):
		return 33
	case strings.Contains(icon, "ic_tank_empty"), strings.Contains(icon, "ic_tank_zero_percent"):
		return 0
	default:
		return -1
	}
}

// SupportsModes reports whether the unit exposes an @MODE datapoint.
func (d *Device) SupportsModes() bool {
	info := d.snapshotInfo()
	_, ok := info["@MODE"]
	return ok
}

// SupportsOnOff reports whether the unit exposes an @ENABLED datapoint
// (used by units that have no mode list — they just have on/off).
func (d *Device) SupportsOnOff() bool {
	info := d.snapshotInfo()
	_, ok := info["@ENABLED"]
	return ok
}

// SupportedModes returns the operating modes this unit advertises. The
// order matches the unit's enumText, so SetMode can look up the right
// index. ModeOff is appended when the unit also supports @ENABLED.
func (d *Device) SupportedModes() []Mode {
	out := make([]Mode, 0, 6)
	labels := d.modeLabels()
	for _, label := range labels {
		m := modeFromEnumText(label)
		if m == ModeUnknown {
			continue
		}
		out = append(out, m)
	}
	if d.SupportsOnOff() {
		// Guarantee Off is in the list.
		hasOff := slices.Contains(out, ModeOff)
		if !hasOff {
			out = append(out, ModeOff)
		}
	}
	return out
}

// modeLabels returns the enumText for the @MODE datapoint, or nil.
func (d *Device) modeLabels() []string {
	info := d.snapshotInfo()
	v, ok := info["@MODE"]
	if !ok {
		return nil
	}
	obj := decodeDatapoint(v)
	if obj == nil {
		return nil
	}
	return obj.Constraints.EnumText
}

// Mode returns the current operating mode. ModeOff is reported if the
// unit supports on/off and @ENABLED is 0.
func (d *Device) Mode() Mode {
	if d.SupportsOnOff() && !d.Enabled() {
		return ModeOff
	}
	if !d.SupportsModes() {
		switch d.GenericType() {
		case "gasWaterHeater", "tanklessWaterHeater":
			return ModeGas
		default:
			return ModeElectric
		}
	}
	labels := d.modeLabels()
	info := d.snapshotInfo()
	v := info["@MODE"]
	obj := decodeDatapoint(v)
	if obj == nil {
		return ModeUnknown
	}
	var idx int
	{
		var n float64
		if err := json.Unmarshal(obj.Value, &n); err == nil {
			idx = int(n)
		}
	}
	if idx < 0 || idx >= len(labels) {
		return ModeUnknown
	}
	return modeFromEnumText(labels[idx])
}

// Enabled reports whether the unit's master on/off datapoint is on.
// For mode-supporting units, it returns false if the active mode is
// "OFF".
func (d *Device) Enabled() bool {
	if d.SupportsModes() {
		labels := d.modeLabels()
		info := d.snapshotInfo()
		obj := decodeDatapoint(info["@MODE"])
		if obj != nil {
			var n float64
			if err := json.Unmarshal(obj.Value, &n); err == nil {
				idx := int(n)
				if idx >= 0 && idx < len(labels) {
					return !strings.EqualFold(labels[idx], "OFF")
				}
			}
		}
	}
	if d.SupportsOnOff() {
		info := d.snapshotInfo()
		obj := decodeDatapoint(info["@ENABLED"])
		if obj == nil {
			return false
		}
		var n float64
		_ = json.Unmarshal(obj.Value, &n)
		return int(n) == 1
	}
	return false
}

// --- Helpers -------------------------------------------------------------

func decodeDatapoint(b json.RawMessage) *datapointObject {
	if len(b) == 0 || !isJSONObject(b) {
		return nil
	}
	var obj datapointObject
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil
	}
	return &obj
}
