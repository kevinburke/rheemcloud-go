// Command rheemcloud is a small CLI around the rheemcloud-go client.
//
// It uses Rheem's cloud (the same service the EcoNet mobile app talks
// to), so all commands go through Rheem's servers — there is no local
// path. You need a Rheem EcoNet account with the heater enrolled.
//
// Subcommands:
//
//	login        Verify credentials and print account ID.
//	devices      List enrolled devices and their key state.
//	state        Print a single device's full state (defaults to the first water heater).
//	raw          Dump the full raw JSON document for a device (every datapoint).
//	energy       Print today's hourly energy-usage chart.
//	water        Print today's hourly water-usage chart.
//	watch        Stream MQTT state updates until interrupted.
//	mode <m>     Set operating mode (off, electric, energy-saving, heat-pump, high-demand, performance).
//	setpoint <F> Set target water temperature in °F.
//	away on|off  Toggle vacation/away mode.
//
// Flags:
//
//	--email     EcoNet account email (default $RHEEM_EMAIL)
//	--password  EcoNet account password (default $RHEEM_PASSWORD)
//	--serial    Target a specific device by serial (default: first water heater)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"time"

	rheemcloud "github.com/kevinburke/rheemcloud-go"
)

func main() {
	var (
		email    = flag.String("email", os.Getenv("RHEEM_EMAIL"), "EcoNet account email (env RHEEM_EMAIL)")
		password = flag.String("password", os.Getenv("RHEEM_PASSWORD"), "EcoNet account password (env RHEEM_PASSWORD)")
		serial   = flag.String("serial", os.Getenv("RHEEM_SERIAL"), "target device serial number (env RHEEM_SERIAL; default: first water heater)")
		timeout  = flag.Duration("timeout", 15*time.Second, "per-operation network timeout")
		printVer = flag.Bool("version", false, "print version and exit")
		verbose  = flag.Bool("v", false, "verbose logging")
		watchDur = flag.Duration("watch-duration", 0, "for 'watch': exit after this long (0 = forever)")
		settle   = flag.Duration("settle", 5*time.Second, "for 'raw': how long to wait for MQTT updates to populate @-fields before dumping")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <subcommand> [args]\n\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nSubcommands:\n")
		fmt.Fprintf(os.Stderr, "  login\n")
		fmt.Fprintf(os.Stderr, "  devices\n")
		fmt.Fprintf(os.Stderr, "  state\n")
		fmt.Fprintf(os.Stderr, "  raw [--settle DURATION]\n")
		fmt.Fprintf(os.Stderr, "  energy\n")
		fmt.Fprintf(os.Stderr, "  water\n")
		fmt.Fprintf(os.Stderr, "  watch\n")
		fmt.Fprintf(os.Stderr, "  mode <off|electric|energy-saving|heat-pump|high-demand|performance>\n")
		fmt.Fprintf(os.Stderr, "  setpoint <degF>\n")
		fmt.Fprintf(os.Stderr, "  away <on|off>\n")
	}
	flag.Parse()
	if *printVer {
		fmt.Println(rheemcloud.Version)
		return
	}

	logLevel := slog.LevelWarn
	if *verbose {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if *email == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "error: --email and --password are required (or RHEEM_EMAIL / RHEEM_PASSWORD env vars)")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	connectCtx, connectCancel := context.WithTimeout(ctx, *timeout)
	client, err := rheemcloud.Connect(connectCtx, *email, *password, &rheemcloud.Config{HTTPTimeout: *timeout})
	connectCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	args := flag.Args()
	switch args[0] {
	case "login":
		fmt.Printf("Authenticated. Account ID: %s\n", client.AccountID())
		fmt.Printf("Devices: %d\n", len(client.Devices()))
	case "devices":
		runDevices(client)
	case "state":
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		// Give the first MQTT messages a moment to land so volatile
		// fields like @SIGNAL/@RUNNING are populated.
		time.Sleep(750 * time.Millisecond)
		printDeviceState(dev)
	case "raw":
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runRaw(ctx, dev, *settle)
	case "energy":
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runEnergy(ctx, client, dev)
	case "water":
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runWater(ctx, client, dev)
	case "action":
		if len(args) < 2 {
			fail("action requires the action name (e.g. waterheaterHealthView)")
		}
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runAction(ctx, client, dev, args[1])
	case "watch":
		runWatch(ctx, client, *watchDur)
	case "mode":
		if len(args) < 2 {
			fail("mode requires an argument")
		}
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runMode(ctx, client, dev, args[1])
	case "setpoint":
		if len(args) < 2 {
			fail("setpoint requires a temperature in °F")
		}
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runSetpoint(ctx, client, dev, args[1])
	case "away":
		if len(args) < 2 {
			fail("away requires on or off")
		}
		dev := pickDevice(client, *serial)
		if dev == nil {
			os.Exit(1)
		}
		runAway(ctx, client, dev, args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		flag.Usage()
		os.Exit(2)
	}
}

func runDevices(c *rheemcloud.Client) {
	devs := c.Devices()
	if len(devs) == 0 {
		fmt.Println("(no devices)")
		return
	}
	serials := make([]string, 0, len(devs))
	for k := range devs {
		serials = append(serials, k)
	}
	sort.Strings(serials)
	for _, s := range serials {
		d := devs[s]
		fmt.Printf("%s\n", s)
		fmt.Printf("  Name:    %s\n", d.FriendlyName())
		fmt.Printf("  Type:    %s (%s)\n", d.Type(), d.GenericType())
		fmt.Printf("  Mode:    %s\n", d.Mode())
		fmt.Printf("  Setpoint:%d °F\n", d.Setpoint())
		fmt.Printf("  Running: %v (%s)\n", d.Running(), d.RunningState())
		fmt.Printf("  Connected: %v\n", d.Connected())
	}
}

func pickDevice(c *rheemcloud.Client, serial string) *rheemcloud.Device {
	if serial != "" {
		dev := c.Device(serial)
		if dev == nil {
			fmt.Fprintf(os.Stderr, "no device with serial %q\n", serial)
		}
		return dev
	}
	for _, d := range c.Devices() {
		if d.Type() == rheemcloud.DeviceWaterHeater {
			return d
		}
	}
	fmt.Fprintln(os.Stderr, "no water heaters on this account; use --serial to pick a specific device")
	return nil
}

func printDeviceState(d *rheemcloud.Device) {
	lo, hi := d.SetpointLimits()
	fmt.Printf("Name:         %s\n", d.FriendlyName())
	fmt.Printf("Serial:       %s\n", d.SerialNumber())
	fmt.Printf("Type:         %s (%s)\n", d.Type(), d.GenericType())
	fmt.Printf("Connected:    %v\n", d.Connected())
	fmt.Printf("Mode:         %s\n", d.Mode())
	fmt.Printf("Setpoint:     %d °F (range %d–%d)\n", d.Setpoint(), lo, hi)
	fmt.Printf("Running:      %v (%s)\n", d.Running(), d.RunningState())
	if hw := d.HotWaterAvailability(); hw >= 0 {
		fmt.Printf("Hot water:    %d%%\n", hw)
	}
	fmt.Printf("Alerts:       %d\n", d.AlertCount())
	fmt.Printf("Wi-Fi (dBm):  %d\n", d.WiFiSignal())
	fmt.Printf("Supports away: %v   Away: %v\n", d.SupportsAway(), d.Away())
	if modes := d.SupportedModes(); len(modes) > 0 {
		labels := make([]string, len(modes))
		for i, m := range modes {
			labels[i] = m.String()
		}
		fmt.Printf("Modes:        %v\n", labels)
	}
}

func runRaw(ctx context.Context, dev *rheemcloud.Device, settle time.Duration) {
	if settle > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(settle):
		}
	}
	raw := dev.Raw()
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Re-marshal into a JSON object preserving sorted key order via an
	// intermediate slice of (key,value) pairs.
	var buf []byte
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',', '\n', ' ', ' ')
		} else {
			buf = append(buf, '\n', ' ', ' ')
		}
		kb, _ := json.Marshal(k)
		buf = append(buf, kb...)
		buf = append(buf, ':', ' ')
		buf = append(buf, raw[k]...)
	}
	buf = append(buf, '\n', '}', '\n')
	// Best-effort pretty-print: marshal/unmarshal through json for clean formatting.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, buf, "", "  "); err == nil {
		_, _ = os.Stdout.Write(pretty.Bytes())
		_, _ = io.WriteString(os.Stdout, "\n")
	} else {
		_, _ = os.Stdout.Write(buf)
	}
}

func runEnergy(ctx context.Context, c *rheemcloud.Client, dev *rheemcloud.Device) {
	eu, err := c.EnergyUsage(ctx, dev, time.Time{}, time.Time{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Today's energy usage (%s):\n", eu.EnergyType)
	if eu.Message != "" {
		fmt.Printf("  %s\n", eu.Message)
	}
	for _, p := range eu.Today {
		fmt.Printf("  %02d:00  %8.3f\n", p.Bucket, p.Value)
	}
	fmt.Printf("  Total: %.3f %s\n", eu.Total, eu.EnergyType)
	if len(eu.Past) > 0 {
		fmt.Printf("\nComparison series (\"past\"):\n")
		for _, p := range eu.Past {
			fmt.Printf("  bucket %2d  %8.3f\n", p.Bucket, p.Value)
		}
	}
}

func runWater(ctx context.Context, c *rheemcloud.Client, dev *rheemcloud.Device) {
	wu, err := c.WaterUsage(ctx, dev, time.Time{}, time.Time{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Today's water usage:\n")
	if wu.Message != "" {
		fmt.Printf("  %s\n", wu.Message)
	}
	for _, p := range wu.Today {
		fmt.Printf("  %02d:00  %8.3f\n", p.Bucket, p.Value)
	}
	fmt.Printf("  Total: %.3f\n", wu.Total)
	if len(wu.Past) > 0 {
		fmt.Printf("\nComparison series (\"past\"):\n")
		for _, p := range wu.Past {
			fmt.Printf("  bucket %2d  %8.3f\n", p.Bucket, p.Value)
		}
	}
}

func runAction(ctx context.Context, c *rheemcloud.Client, dev *rheemcloud.Device, action string) {
	payload := map[string]string{
		"ACTION":        action,
		"device_name":   dev.DeviceID(),
		"serial_number": dev.SerialNumber(),
	}
	raw, err := c.DynamicAction(ctx, payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		_, _ = os.Stdout.Write(pretty.Bytes())
		_, _ = io.WriteString(os.Stdout, "\n")
	} else {
		_, _ = os.Stdout.Write(raw)
	}
}

func runWatch(ctx context.Context, c *rheemcloud.Client, limit time.Duration) {
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	events := c.Subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			ts := ev.At.Format(time.TimeOnly)
			switch ev.Kind {
			case rheemcloud.EventConnected:
				fmt.Printf("[%s] connected\n", ts)
			case rheemcloud.EventDisconnected:
				fmt.Printf("[%s] disconnected\n", ts)
			case rheemcloud.EventStateChange:
				fmt.Printf("[%s] %s: mode=%s setpoint=%d running=%v\n",
					ts, ev.Device.FriendlyName(), ev.Device.Mode(),
					ev.Device.Setpoint(), ev.Device.Running())
			}
		}
	}
}

func runMode(ctx context.Context, c *rheemcloud.Client, dev *rheemcloud.Device, name string) {
	var m rheemcloud.Mode
	switch name {
	case "off":
		m = rheemcloud.ModeOff
	case "electric":
		m = rheemcloud.ModeElectric
	case "energy-saving", "eco":
		m = rheemcloud.ModeEnergySaving
	case "heat-pump", "heatpump", "hp":
		m = rheemcloud.ModeHeatPump
	case "high-demand", "highdemand", "hd":
		m = rheemcloud.ModeHighDemand
	case "performance":
		m = rheemcloud.ModePerformance
	case "gas":
		m = rheemcloud.ModeGas
	default:
		fail(fmt.Sprintf("unknown mode %q", name))
	}
	if err := c.SetMode(ctx, dev, m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runSetpoint(ctx context.Context, c *rheemcloud.Client, dev *rheemcloud.Device, arg string) {
	v, err := strconv.Atoi(arg)
	if err != nil {
		fail(fmt.Sprintf("setpoint: %v", err))
	}
	if err := c.SetSetpoint(ctx, dev, v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runAway(ctx context.Context, c *rheemcloud.Client, dev *rheemcloud.Device, arg string) {
	var on bool
	switch arg {
	case "on", "true":
		on = true
	case "off", "false":
		on = false
	default:
		fail(fmt.Sprintf("away: expected on or off, got %q", arg))
	}
	if err := c.SetAway(ctx, dev, on); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(2)
}
