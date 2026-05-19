# rheemcloud-go

A Go client for the **Rheem EcoNet cloud** — the same backend the
official EcoNet iOS/Android app uses to control Rheem water heaters and
thermostats.

Auth and the device list go over HTTPS to `rheem.clearblade.com`
(a ClearBlade-hosted BaaS). State updates and commands flow over
MQTT/TLS to the same host on port 1884. There is no Rheem-published
API; this package follows the protocol the
[pyeconet](https://github.com/w1ll1am23/pyeconet) project
reverse-engineered from Rheem's Android app.

You need a Rheem EcoNet account (the one you use to log into the app)
and the heater's Wi-Fi module enrolled. Every command traverses Rheem's
servers — this is **not** a local-control client. For local control of
heat-pump water heaters without internet, see the sibling
[`econet-go`](https://github.com/kevinburke/econet-go) package, which
targets the
[esphome-econet](https://github.com/esphome-econet/esphome-econet)
RS485 bridge.

## Install

```
go get github.com/kevinburke/rheemcloud-go
go install github.com/kevinburke/rheemcloud-go/cmd/rheemcloud@latest
```

## Library use

```go
ctx := context.Background()
client, err := rheemcloud.Connect(ctx, os.Getenv("RHEEM_EMAIL"), os.Getenv("RHEEM_PASSWORD"), nil)
if err != nil {
    log.Fatal(err)
}
defer client.Close()

for serial, dev := range client.Devices() {
    log.Printf("%s — %s, mode=%s setpoint=%d°F", serial, dev.FriendlyName(), dev.Mode(), dev.Setpoint())
}

// Subscribe to MQTT state changes:
for ev := range client.Subscribe() {
    if ev.Kind == rheemcloud.EventStateChange {
        log.Printf("%s: mode=%s running=%v", ev.Device.FriendlyName(), ev.Device.Mode(), ev.Device.Running())
    }
}
```

`Client.Devices()` returns a snapshot of known equipment keyed by
serial number. `Subscribe()` returns a channel that delivers every
state delta plus connect/disconnect events. Setpoint and mode commands
publish to `user/{account_id}/device/desired` over MQTT — the Wi-Fi
module typically applies the change within a few seconds and echoes
back a new `reported` payload.

See the
[package docs](https://pkg.go.dev/github.com/kevinburke/rheemcloud-go)
for the full API.

## Command-line tool

```
export RHEEM_EMAIL='you@example.com'
export RHEEM_PASSWORD='...'

rheemcloud login                  # verify creds, print account_id
rheemcloud devices                # list enrolled devices
rheemcloud state                  # print full state of the first water heater
rheemcloud watch                  # stream MQTT updates until ^C
rheemcloud mode heat-pump         # off | electric | energy-saving | heat-pump | high-demand | performance
rheemcloud setpoint 125           # target temperature in °F
rheemcloud away on                # enable vacation/away mode
```

Pass `--serial SN-XXXX` to target a specific unit if your account has
more than one. Run `rheemcloud --help` for the full flag list.

## Stability caveats

- The Rheem cloud API is undocumented and unsupported. Endpoints,
  field names, or auth flow can change at any time — pyeconet has
  occasionally needed updates after Rheem-side changes.
- Some fields (notably `@SIGNAL`, `@RUNNING`) only arrive on MQTT, not
  in the initial REST device dump. Give a `state` call ~1s after
  `Connect` (the CLI's `state` subcommand does this for you) or call
  `Subscribe()` and wait for the first event.
- Mode label normalization follows pyeconet's `by_string`, which is
  forgiving but not exhaustive — if your unit returns an unrecognized
  label, `Mode()` returns `ModeUnknown` and `SetMode` for that label
  won't work. Open an issue with the raw label and we'll add it.

## License

MIT
