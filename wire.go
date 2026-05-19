package rheemcloud

import "encoding/json"

// Static credentials embedded in the Rheem EcoNet Android app — the
// ClearBlade BaaS uses them to identify the calling app independent of
// the per-user auth. pyeconet uses the same values.
const (
	clearBladeSystemKey    = "e2e699cb0bb0bbb88fc8858cb5a401"
	clearBladeSystemSecret = "E2E699CB0BE6C6FADDB1B0BC9A20"
	restHost               = "rheem.clearblade.com"
	mqttHost               = "rheem.clearblade.com"
	mqttPort               = 1884
)

// --- REST: POST /user/auth ----------------------------------------------

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// authResponse matches the shape pyeconet expects: at the top level a
// user_token, and the per-user options block carries the actual success
// flag and account_id. (Yes, "success" is nested inside "options" — not
// at the top level — and the top-level success bool is unreliable.)
type authResponse struct {
	UserToken string `json:"user_token"`
	Options   struct {
		Success   bool   `json:"success"`
		AccountID string `json:"account_id"`
		Message   string `json:"message"`
	} `json:"options"`
}

// --- REST: POST /code/{systemKey}/getUserDataForApp ---------------------

type userDataResponse struct {
	Success bool `json:"success"`
	Results struct {
		// pyeconet always passes {"resource": "friedrich"} as the
		// request body. The locations array is what we care about.
		Locations []location `json:"locations"`
	} `json:"results"`
}

type location struct {
	// "equiptments" is misspelled in the upstream API; matching it
	// exactly is required for unmarshal.
	Equipments []equipmentInfo `json:"equiptments"`
}

// equipmentInfo is one row from the user-data response. The fields
// prefixed with "@" are reported state datapoints; their shape varies
// (some are bare strings/bools/ints, some are objects with a `value`
// and `constraints`). We hold them as raw JSON and resolve per access.
type equipmentInfo struct {
	DeviceName   string          `json:"device_name"`
	DeviceType   string          `json:"device_type"` // "WH" or "HVAC"
	SerialNumber string          `json:"serial_number"`
	Error        json.RawMessage `json:"error,omitempty"`

	// Capture every other key (including the "@" datapoints) as raw
	// JSON so per-property accessors can inspect their actual shape.
	Raw map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the typed fields populated and stashes every
// remaining key into Raw.
func (e *equipmentInfo) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	e.Raw = m
	if v, ok := m["device_name"]; ok {
		_ = json.Unmarshal(v, &e.DeviceName)
	}
	if v, ok := m["device_type"]; ok {
		_ = json.Unmarshal(v, &e.DeviceType)
	}
	if v, ok := m["serial_number"]; ok {
		_ = json.Unmarshal(v, &e.SerialNumber)
	}
	if v, ok := m["error"]; ok {
		e.Error = v
	}
	return nil
}

// --- Datapoint shapes ----------------------------------------------------

// A typical "@SETPOINT" looks like:
//
//	{"value": 125, "status": "...", "constraints": {"lowerLimit": 95, "upperLimit": 140}}
//
// Some datapoints are bare scalars (booleans or strings). The accessors
// in device.go probe both shapes.

type datapointObject struct {
	Value       json.RawMessage `json:"value"`
	Status      string          `json:"status"`
	Title       string          `json:"title"`
	Constraints datapointConstraints
}

type datapointConstraints struct {
	LowerLimit float64  `json:"lowerLimit"`
	UpperLimit float64  `json:"upperLimit"`
	EnumText   []string `json:"enumText"`
}

func (d *datapointObject) UnmarshalJSON(b []byte) error {
	var m struct {
		Value       json.RawMessage      `json:"value"`
		Status      string               `json:"status"`
		Title       string               `json:"title"`
		Constraints datapointConstraints `json:"constraints"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	d.Value = m.Value
	d.Status = m.Status
	d.Title = m.Title
	d.Constraints = m.Constraints
	return nil
}
