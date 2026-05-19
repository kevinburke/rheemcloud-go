package rheemcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// UsagePoint is one bucket of historical usage data. For daily reports
// Hour is 0-23. For the historical-week series Rheem returns alongside
// the day, the same struct holds 7 days indexed 1-7 (Mon=1?) — the
// upstream API doesn't document the labels so consumers should treat
// the bucket key as an opaque integer.
type UsagePoint struct {
	Bucket int     // hour-of-day for today's data, day-of-week for the history series
	Value  float64 // kWh, kBTU, or gallons depending on the usage type
}

// EnergyUsage reports the unit's energy consumption for a single day.
// Today is the default if start/end are zero. The cloud returns two
// series: Today (one point per hour, midnight-to-midnight in the unit's
// local time) and Past (a comparison series the EcoNet app overlays on
// the "today" chart — semantics not officially documented).
type EnergyUsage struct {
	Today []UsagePoint
	Past  []UsagePoint
	// Total is the sum of Today's points — kWh for electric/heat-pump
	// units, kBTU for gas, per the EnergyType field.
	Total float64
	// EnergyType is the unit reported by the API — typically "KWH" or
	// "KBTU". Inferred from the human-readable "message" field.
	EnergyType string
	// Message is the human-readable summary string from Rheem (e.g.
	// "You used 1.23 kWh of energy today.").
	Message string
}

// WaterUsage reports the unit's water draw for a single day.
type WaterUsage struct {
	Today []UsagePoint
	Past  []UsagePoint
	// Total is the sum of Today's points, in the units Rheem returns
	// (typically gallons).
	Total float64
	// Message is the human-readable summary from Rheem.
	Message string
}

// EnergyUsage queries the cloud for the device's hourly energy usage
// over the supplied window. Pass zero start/end to default to "today
// 00:00:00.999" through "today 23:59:59.999" in the local timezone,
// matching pyeconet's defaults.
func (c *Client) EnergyUsage(ctx context.Context, dev *Device, start, end time.Time) (*EnergyUsage, error) {
	if dev == nil {
		return nil, ErrNoDevice
	}
	startStr, endStr := defaultUsageWindow(start, end, true)
	payload := map[string]string{
		"ACTION":        "waterheaterUsageReportView",
		"device_name":   dev.DeviceID(),
		"serial_number": dev.SerialNumber(),
		"start_date":    startStr,
		"end_date":      endStr,
		"usage_type":    "energyUsage",
	}
	resp, err := c.dynamicAction(ctx, payload)
	if err != nil {
		return nil, err
	}
	var dr struct {
		Results struct {
			EnergyUsage struct {
				Data        []rawUsagePoint `json:"data"`
				HistoryData []rawUsagePoint `json:"historyData"`
				Message     string          `json:"message"`
			} `json:"energy_usage"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &dr); err != nil {
		return nil, fmt.Errorf("rheemcloud: parse energy usage: %w", err)
	}
	eu := &EnergyUsage{
		Today:      parseUsagePoints(dr.Results.EnergyUsage.Data),
		Past:       parseUsagePoints(dr.Results.EnergyUsage.HistoryData),
		Message:    dr.Results.EnergyUsage.Message,
		EnergyType: energyTypeFromMessage(dr.Results.EnergyUsage.Message, dev.GenericType()),
	}
	for _, p := range eu.Today {
		eu.Total += p.Value
	}
	return eu, nil
}

// WaterUsage queries the cloud for the device's hourly water usage.
// Same windowing semantics as EnergyUsage. Returns ErrNotSupported (via
// the typed error) if the unit doesn't track water consumption.
func (c *Client) WaterUsage(ctx context.Context, dev *Device, start, end time.Time) (*WaterUsage, error) {
	if dev == nil {
		return nil, ErrNoDevice
	}
	startStr, endStr := defaultUsageWindow(start, end, false)
	payload := map[string]string{
		"ACTION":        "waterheaterUsageReportView",
		"device_name":   dev.DeviceID(),
		"serial_number": dev.SerialNumber(),
		"start_date":    startStr,
		"end_date":      endStr,
		"usage_type":    "waterUsage",
	}
	resp, err := c.dynamicAction(ctx, payload)
	if err != nil {
		return nil, err
	}
	var dr struct {
		Results struct {
			WaterUsage struct {
				Data        []rawUsagePoint `json:"data"`
				HistoryData []rawUsagePoint `json:"historyData"`
				Message     string          `json:"message"`
			} `json:"water_usage"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp, &dr); err != nil {
		return nil, fmt.Errorf("rheemcloud: parse water usage: %w", err)
	}
	wu := &WaterUsage{
		Today:   parseUsagePoints(dr.Results.WaterUsage.Data),
		Past:    parseUsagePoints(dr.Results.WaterUsage.HistoryData),
		Message: dr.Results.WaterUsage.Message,
	}
	for _, p := range wu.Today {
		wu.Total += p.Value
	}
	return wu, nil
}

type rawUsagePoint struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

func parseUsagePoints(in []rawUsagePoint) []UsagePoint {
	out := make([]UsagePoint, 0, len(in))
	for _, p := range in {
		n, err := strconv.Atoi(p.Name)
		if err != nil {
			continue
		}
		out = append(out, UsagePoint{Bucket: n, Value: p.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out
}

// DynamicAction is the low-level escape hatch: it POSTs an arbitrary
// payload to /code/{systemKey}/dynamicAction and returns the raw
// response JSON. The shape of both request and response varies by
// action — see pyeconet or sniff the EcoNet app for examples. Useful
// for exploring undocumented actions like waterheaterHealthView.
func (c *Client) DynamicAction(ctx context.Context, payload any) ([]byte, error) {
	return c.dynamicAction(ctx, payload)
}

// dynamicAction POSTs to /code/{systemKey}/dynamicAction and returns
// the raw response JSON. The response shape varies per action; callers
// unmarshal into their own struct.
//
// This bypasses c.rest.Do because we want both the envelope's success
// flag AND the raw bytes — c.rest.Do is built for "parse into v" and
// doesn't expose the underlying body.
func (c *Client) dynamicAction(ctx context.Context, payload any) ([]byte, error) {
	c.mu.Lock()
	token := c.userToken
	c.mu.Unlock()
	if token == "" {
		return nil, ErrClosed
	}
	path := fmt.Sprintf("/code/%s/dynamicAction", clearBladeSystemKey)
	req, err := c.newRESTRequest(ctx, http.MethodPost, path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ClearBlade-UserToken", token)
	resp, err := c.rest.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rheemcloud: dynamicAction: HTTP %d", resp.StatusCode)
	}
	var env struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("rheemcloud: dynamicAction: parse envelope: %w", err)
	}
	if !env.Success {
		if env.Error != "" {
			return nil, fmt.Errorf("rheemcloud: dynamicAction failed: %s", env.Error)
		}
		return nil, fmt.Errorf("rheemcloud: dynamicAction failed (no error message)")
	}
	return raw, nil
}

// defaultUsageWindow returns ISO-8601 start/end strings covering the
// current local day. pyeconet uses two slightly different formats:
// energy uses isoformat(timespec='milliseconds') (offset included)
// while water uses a hand-rolled strftime without offset. We match
// pyeconet exactly because the upstream parser is picky about format.
func defaultUsageWindow(start, end time.Time, energy bool) (string, string) {
	now := time.Now()
	if start.IsZero() {
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 999_000_000, now.Location())
	}
	if end.IsZero() {
		end = time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999_000_000, now.Location())
	}
	if energy {
		// Python's isoformat(timespec='milliseconds') with tzinfo:
		// "2026-05-18T00:00:00.999-07:00" — Go's RFC3339 with .000 and
		// numeric zone is the closest match.
		return formatPyIsoMs(start), formatPyIsoMs(end)
	}
	// water_usage uses "%Y-%m-%dT%H:%M:%S.999" with no timezone.
	const f = "2006-01-02T15:04:05.999"
	return start.Format(f), end.Format(f)
}

// formatPyIsoMs emulates Python's datetime.isoformat(timespec='milliseconds')
// for a tz-aware datetime: YYYY-MM-DDTHH:MM:SS.mmm±HH:MM.
func formatPyIsoMs(t time.Time) string {
	// Go's "2006-01-02T15:04:05.000Z07:00" gives the right shape, but
	// the zone is "-0700" without the colon. Add it back.
	s := t.Format("2006-01-02T15:04:05.000-0700")
	if len(s) >= 5 {
		// "...-0700" → "...-07:00"
		s = s[:len(s)-2] + ":" + s[len(s)-2:]
	}
	return s
}

// energyTypeFromMessage extracts a unit label like "KWH" or "KBTU" from
// the human-readable message Rheem returns. The 4th whitespace-
// separated word is the unit per pyeconet's heuristic ("You used X.Y
// kWh of energy today.").
func energyTypeFromMessage(msg, genericType string) string {
	fields := splitOnSpace(msg)
	if len(fields) >= 4 {
		return upperString(fields[3])
	}
	if genericType == "gasWaterHeater" {
		return "KBTU"
	}
	return "KWH"
}

func splitOnSpace(s string) []string {
	out := []string{}
	cur := []byte{}
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			continue
		}
		cur = append(cur, s[i])
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

func upperString(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
