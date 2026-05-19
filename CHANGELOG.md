# Changelog

## v0.1.0 (2026-05-19)

- Initial release.
- Go client for the Rheem EcoNet cloud — the same service the official
  mobile app talks to. REST auth + device list against
  `rheem.clearblade.com`; MQTT/TLS state stream on port 1884.
- Protocol modelled on the open-source
  [pyeconet](https://github.com/w1ll1am23/pyeconet) library.
- Water-heater support: `Connect`, `Devices`, `Device.Mode`,
  `Device.Setpoint`/`SetpointLimits`, `Device.Running`,
  `Device.HotWaterAvailability`, `Subscribe`, `Device.Raw`.
- Commands: `SetMode`, `SetSetpoint`, `SetAway`.
- Usage queries: `EnergyUsage`, `WaterUsage`, and the low-level
  `DynamicAction` escape hatch for other undocumented actions
  (`waterheaterHealthView`, `waterheaterScheduleView`,
  `networkSettings`, etc).
- `cmd/rheemcloud` CLI: `login`, `devices`, `state`, `raw`, `energy`,
  `water`, `action`, `watch`, `mode`, `setpoint`, `away`.

### Verified end-to-end against a real Rheem Gen5 HPWH

- Authentication, device discovery, MQTT subscribe, and the energy
  usage query all work against a live account.

### Known gaps

- `SetMode`, `SetSetpoint`, `SetAway` are unit-tested for payload
  shape but were not published to a real device in development.
- MQTT state-delta parsing is unit-tested; no real state change
  arrived during the 10-second live watch (unit was idle).
- Reconnect/backoff relies on paho-mqtt's `SetAutoReconnect`; not
  exercised under a real disconnect.
