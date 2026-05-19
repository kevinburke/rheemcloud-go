package rheemcloud

import (
	"encoding/json"
	"testing"
)

func TestModeFromEnumText(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"Off", ModeOff},
		{"OFF", ModeOff},
		{"Electric", ModeElectric},
		{"Electric Mode", ModeElectric},
		{"Energy Saver", ModeEnergySaving},
		{"Energy Saving", ModeEnergySaving},
		{"Heat Pump", ModeHeatPump},
		{"Heat Pump Only", ModeHeatPump},
		{"High Demand", ModeHighDemand},
		{"Performance", ModePerformance},
		{"Vacation", ModeVacation},
		{"Gas", ModeGas},
		{"  Heat Pump  ", ModeUnknown}, // leading whitespace not stripped — matches pyeconet's rstrip-only behavior
		{"Heat Pump  ", ModeHeatPump},
		{"Bogus", ModeUnknown},
		{"", ModeUnknown},
	}
	for _, tc := range cases {
		if got := modeFromEnumText(tc.in); got != tc.want {
			t.Errorf("modeFromEnumText(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDeviceModeFromHPWHFixture(t *testing.T) {
	// Synthetic fixture modelled on a Rheem HPWH equipment block: @MODE
	// is an object with value (current index), constraints.enumText
	// (mode labels), and @ENABLED present.
	info := mustRaw(t, map[string]any{
		"device_name":   "device-123",
		"serial_number": "SN-9000",
		"device_type":   "WH",
		"@TYPE":         "heatpumpWaterHeater",
		"@SETPOINT": map[string]any{
			"value":       125,
			"constraints": map[string]any{"lowerLimit": 95, "upperLimit": 140},
		},
		"@MODE": map[string]any{
			"value": 2,
			"constraints": map[string]any{
				"enumText": []string{"Energy Saver", "Heat Pump", "High Demand", "Electric"},
			},
		},
		"@ENABLED":   map[string]any{"value": 1},
		"@RUNNING":   "Heat Pump On",
		"@CONNECTED": true,
	})
	d := newDevice(nil, info)
	if got := d.Mode(); got != ModeHighDemand {
		t.Errorf("Mode() = %v, want %v", got, ModeHighDemand)
	}
	if got := d.Setpoint(); got != 125 {
		t.Errorf("Setpoint() = %d, want 125", got)
	}
	lo, hi := d.SetpointLimits()
	if lo != 95 || hi != 140 {
		t.Errorf("SetpointLimits = %d-%d, want 95-140", lo, hi)
	}
	if !d.Running() {
		t.Errorf("Running() should be true (got %v)", d.RunningState())
	}
	if !d.Connected() {
		t.Errorf("Connected() should be true")
	}
	supported := d.SupportedModes()
	wantSet := map[Mode]bool{ModeEnergySaving: true, ModeHeatPump: true, ModeHighDemand: true, ModeElectric: true, ModeOff: true}
	if len(supported) != len(wantSet) {
		t.Errorf("SupportedModes len = %d, want %d (%v)", len(supported), len(wantSet), supported)
	}
	for _, m := range supported {
		if !wantSet[m] {
			t.Errorf("unexpected mode in SupportedModes: %v", m)
		}
		delete(wantSet, m)
	}
}

func TestEnabledFalseTurnsModeOffWithoutModeList(t *testing.T) {
	// On units with @ENABLED but no @MODE, @ENABLED=0 should produce
	// Mode==Off. (When @MODE is present it wins over @ENABLED — see
	// pyeconet's WaterHeater.enabled / .mode.)
	info := mustRaw(t, map[string]any{
		"device_name":   "d",
		"serial_number": "s",
		"device_type":   "WH",
		"@TYPE":         "electricWaterHeater",
		"@ENABLED":      map[string]any{"value": 0},
	})
	d := newDevice(nil, info)
	if got := d.Mode(); got != ModeOff {
		t.Errorf("Mode() with @ENABLED=0 (no @MODE) should be Off, got %v", got)
	}
}

func TestModeListSelectionViaModeIndex(t *testing.T) {
	// Active mode is determined by @MODE.value indexing into enumText
	// even if @ENABLED is also present.
	info := mustRaw(t, map[string]any{
		"device_name":   "d",
		"serial_number": "s",
		"device_type":   "WH",
		"@ENABLED":      map[string]any{"value": 1},
		"@MODE": map[string]any{
			"value":       0,
			"constraints": map[string]any{"enumText": []string{"OFF", "Heat Pump", "High Demand"}},
		},
	})
	d := newDevice(nil, info)
	if got := d.Mode(); got != ModeOff {
		t.Errorf("Mode() with @MODE index pointing at OFF should be Off, got %v", got)
	}
}

func TestHotWaterAvailability(t *testing.T) {
	cases := []struct {
		icon string
		want int
	}{
		{"ic_tank_hundread_percent", 100},
		{"ic_tank_fourty_percent", 66},
		{"ic_tank_ten_percent", 33},
		{"ic_tank_empty", 0},
		{"ic_tank_zero_percent", 0},
		{"unrecognized_icon_name", -1},
	}
	for _, tc := range cases {
		info := mustRaw(t, map[string]any{"@HOTWATER": tc.icon})
		d := newDevice(nil, info)
		if got := d.HotWaterAvailability(); got != tc.want {
			t.Errorf("HotWaterAvailability(%q) = %d, want %d", tc.icon, got, tc.want)
		}
	}
}

func TestApplyUpdateMergesValueIntoExistingObject(t *testing.T) {
	info := mustRaw(t, map[string]any{
		"device_name":   "d",
		"serial_number": "s",
		"device_type":   "WH",
		"@SETPOINT": map[string]any{
			"value":       125,
			"constraints": map[string]any{"lowerLimit": 95, "upperLimit": 140},
		},
	})
	d := newDevice(nil, info)
	update := mustRaw(t, map[string]any{
		"device_name": "d",
		"@SETPOINT":   130,
	})
	if !d.applyUpdate(update) {
		t.Fatal("applyUpdate returned false; expected a change")
	}
	if got := d.Setpoint(); got != 130 {
		t.Errorf("Setpoint after update = %d, want 130", got)
	}
	lo, hi := d.SetpointLimits()
	if lo != 95 || hi != 140 {
		t.Errorf("constraints clobbered: %d-%d, want 95-140", lo, hi)
	}
}

func TestApplyUpdateMergesObjectIntoObject(t *testing.T) {
	info := mustRaw(t, map[string]any{
		"device_name": "d",
		"@MODE": map[string]any{
			"value":       0,
			"constraints": map[string]any{"enumText": []string{"Off", "Heat Pump"}},
		},
	})
	d := newDevice(nil, info)
	update := mustRaw(t, map[string]any{
		"device_name": "d",
		"@MODE":       map[string]any{"value": 1, "status": "Heat Pump"},
	})
	if !d.applyUpdate(update) {
		t.Fatal("applyUpdate returned false")
	}
	if got := d.Mode(); got != ModeHeatPump {
		t.Errorf("Mode = %v, want HeatPump", got)
	}
}

func TestApplyUpdateIgnoresNonDatapointKeys(t *testing.T) {
	info := mustRaw(t, map[string]any{"device_name": "d", "@SETPOINT": 120})
	d := newDevice(nil, info)
	update := mustRaw(t, map[string]any{"transactionId": "ANDROID_xyz", "device_name": "d"})
	if d.applyUpdate(update) {
		t.Error("applyUpdate returned true; no @-keys in update so no change expected")
	}
}

func mustRaw(t *testing.T, v map[string]any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
