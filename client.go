package rheemcloud

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/kevinburke/rest/restclient"
)

// Errors returned by Client methods.
var (
	// ErrInvalidCredentials is returned by Connect when Rheem rejects the
	// email/password pair.
	ErrInvalidCredentials = errors.New("rheemcloud: invalid credentials")
	// ErrClosed is returned by commands issued after Close.
	ErrClosed = errors.New("rheemcloud: client closed")
	// ErrNoDevice is returned when a command references an unknown
	// device.
	ErrNoDevice = errors.New("rheemcloud: device not found")
	// ErrModeUnsupported is returned by SetMode when the unit doesn't
	// expose the requested mode.
	ErrModeUnsupported = errors.New("rheemcloud: mode not supported by this unit")
	// ErrSetpointOutOfRange is returned by SetSetpoint when the value
	// is outside the unit's reported range.
	ErrSetpointOutOfRange = errors.New("rheemcloud: setpoint out of range")
)

const (
	defaultHTTPTimeout    = 15 * time.Second
	defaultMQTTKeepalive  = 60 * time.Second
	defaultSubscriberCap  = 64
	defaultUserAgent      = "rheemcloud-go"
	initialReconnectDelay = 1 * time.Second
	maxReconnectDelay     = 60 * time.Second
)

// Config holds optional knobs.
type Config struct {
	// UserAgent overrides the User-Agent header on REST calls.
	UserAgent string
	// HTTPTimeout bounds individual REST calls. Defaults to 15s.
	HTTPTimeout time.Duration
	// Logger handles reconnect/error logging. Defaults to slog.Default().
	Logger *slog.Logger
	// HTTPClient lets callers inject a custom http.Client (e.g. for
	// tests). If nil a new client with HTTPTimeout is created.
	HTTPClient *http.Client
}

// Client is a long-lived authenticated EcoNet session.
type Client struct {
	cfg    Config
	rest   *restclient.Client
	logger *slog.Logger

	mu        sync.Mutex
	userToken string
	accountID string
	devices   map[string]*Device // keyed by serial number
	mqtt      mqtt.Client
	closed    bool

	subMu       sync.Mutex
	subscribers []chan Event

	// reconnectN counts MQTT reconnect attempts; used by tests.
	reconnectN atomic.Int64
}

// Connect establishes a full EcoNet session: it authenticates with the
// REST API, fetches the device list, and opens the MQTT subscription
// for state updates. Returns a ready-to-use Client.
//
// ctx bounds setup only — the initial REST auth, REST device list, and
// MQTT handshake. Once Connect returns successfully, the Client is
// independent of ctx; its background MQTT session reconnects on drop
// until Close is called. To bound the client's lifetime, defer Close.
func Connect(ctx context.Context, email, password string, cfg *Config) (*Client, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	timeout := cfg.HTTPTimeout
	if timeout == 0 {
		timeout = defaultHTTPTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	rc := restclient.New("", "", fmt.Sprintf("https://%s/api/v/1", restHost))
	rc.Client = hc
	rc.UploadType = restclient.JSON
	c := &Client{
		cfg:     *cfg,
		rest:    rc,
		logger:  logger,
		devices: map[string]*Device{},
	}
	if err := c.authenticate(ctx, email, password); err != nil {
		return nil, err
	}
	if err := c.refreshDevicesLocked(ctx); err != nil {
		return nil, fmt.Errorf("rheemcloud: load devices: %w", err)
	}
	if err := c.connectMQTT(ctx, email); err != nil {
		return nil, fmt.Errorf("rheemcloud: mqtt connect: %w", err)
	}
	return c, nil
}

// Close disconnects MQTT and releases subscriber channels.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	client := c.mqtt
	c.mqtt = nil
	c.mu.Unlock()

	if client != nil {
		client.Disconnect(250)
	}

	c.subMu.Lock()
	for _, ch := range c.subscribers {
		close(ch)
	}
	c.subscribers = nil
	c.subMu.Unlock()
	return nil
}

// AccountID returns the authenticated user's account ID.
func (c *Client) AccountID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountID
}

// Devices returns a snapshot of currently-known devices, keyed by
// serial number.
func (c *Client) Devices() map[string]*Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*Device, len(c.devices))
	maps.Copy(out, c.devices)
	return out
}

// Device looks up a single device by serial number.
func (c *Client) Device(serial string) *Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.devices[serial]
}

// Refresh re-fetches the device list from REST and merges into the
// in-memory state. Useful if a unit was added or replaced.
func (c *Client) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshDevicesLocked(ctx)
}

// --- REST ---------------------------------------------------------------

func (c *Client) authenticate(ctx context.Context, email, password string) error {
	req, err := c.newRESTRequest(ctx, http.MethodPost, "/user/auth", authRequest{Email: email, Password: password})
	if err != nil {
		return err
	}
	var ar authResponse
	if err := c.rest.Do(req, &ar); err != nil {
		return fmt.Errorf("rheemcloud: auth: %w", err)
	}
	if !ar.Options.Success {
		if ar.Options.Message != "" {
			return fmt.Errorf("%w: %s", ErrInvalidCredentials, ar.Options.Message)
		}
		return ErrInvalidCredentials
	}
	if ar.UserToken == "" || ar.Options.AccountID == "" {
		return fmt.Errorf("rheemcloud: auth: empty user_token or account_id in response")
	}
	c.mu.Lock()
	c.userToken = ar.UserToken
	c.accountID = ar.Options.AccountID
	c.mu.Unlock()
	return nil
}

// refreshDevicesLocked must be called with c.mu held.
func (c *Client) refreshDevicesLocked(ctx context.Context) error {
	path := fmt.Sprintf("/code/%s/getUserDataForApp", clearBladeSystemKey)
	req, err := c.newRESTRequest(ctx, http.MethodPost, path, map[string]string{"resource": "friedrich"})
	if err != nil {
		return err
	}
	req.Header.Set("ClearBlade-UserToken", c.userToken)
	var ud userDataResponse
	if err := c.rest.Do(req, &ud); err != nil {
		return fmt.Errorf("rheemcloud: device list: %w", err)
	}
	for _, loc := range ud.Results.Locations {
		for _, eq := range loc.Equipments {
			if len(eq.Error) > 0 && string(eq.Error) != "null" {
				c.logger.Debug("rheemcloud: skipping equipment with error", "error", string(eq.Error))
				continue
			}
			if eq.SerialNumber == "" {
				continue
			}
			if dev, ok := c.devices[eq.SerialNumber]; ok {
				dev.mu.Lock()
				dev.info = eq.Raw
				dev.LastUpdated = time.Now()
				dev.mu.Unlock()
			} else {
				c.devices[eq.SerialNumber] = newDevice(c, eq.Raw)
			}
		}
	}
	return nil
}

// newRESTRequest builds a request for c.rest with body serialized as
// JSON and the static ClearBlade headers attached. Add per-call headers
// (e.g. ClearBlade-UserToken) on the returned *http.Request before
// calling c.rest.Do. Pass nil body for GETs.
func (c *Client) newRESTRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := c.rest.NewRequestWithContext(ctx, method, path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ClearBlade-SystemKey", clearBladeSystemKey)
	req.Header.Set("ClearBlade-SystemSecret", clearBladeSystemSecret)
	if ua := c.cfg.UserAgent; ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	return req, nil
}

// --- MQTT ---------------------------------------------------------------

func (c *Client) connectMQTT(ctx context.Context, email string) error {
	clientID := fmt.Sprintf("%s%d_android", email, time.Now().UnixMilli())
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("ssl://%s:%d", mqttHost, mqttPort))
	opts.SetClientID(clientID)
	opts.SetCleanSession(true)
	opts.SetUsername(c.userToken)
	opts.SetPassword(clearBladeSystemKey)
	opts.SetKeepAlive(defaultMQTTKeepalive)
	opts.SetTLSConfig(&tls.Config{ServerName: mqttHost})
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetMaxReconnectInterval(maxReconnectDelay)
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		c.logger.Warn("rheemcloud: mqtt disconnected", "err", err)
		c.broadcast(Event{Kind: EventDisconnected, At: time.Now()})
	})
	opts.SetReconnectingHandler(func(_ mqtt.Client, _ *mqtt.ClientOptions) {
		c.reconnectN.Add(1)
	})
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		acct := c.AccountID()
		reportedTopic := "user/" + acct + "/device/reported"
		desiredTopic := "user/" + acct + "/device/desired"
		if tok := client.Subscribe(reportedTopic, 0, c.onMQTTMessage); tok.Wait() && tok.Error() != nil {
			c.logger.Warn("rheemcloud: subscribe failed", "topic", reportedTopic, "err", tok.Error())
		}
		if tok := client.Subscribe(desiredTopic, 0, c.onMQTTMessage); tok.Wait() && tok.Error() != nil {
			c.logger.Warn("rheemcloud: subscribe failed", "topic", desiredTopic, "err", tok.Error())
		}
		c.broadcast(Event{Kind: EventConnected, At: time.Now()})
	})

	mc := mqtt.NewClient(opts)
	token := mc.Connect()
	select {
	case <-token.Done():
		if err := token.Error(); err != nil {
			return err
		}
	case <-ctx.Done():
		// Best-effort cleanup so the connect goroutine doesn't linger.
		mc.Disconnect(0)
		return ctx.Err()
	}
	c.mu.Lock()
	c.mqtt = mc
	c.mu.Unlock()
	return nil
}

func (c *Client) onMQTTMessage(_ mqtt.Client, msg mqtt.Message) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		c.logger.Warn("rheemcloud: mqtt parse failed", "err", err, "topic", msg.Topic())
		return
	}
	var serial string
	if v, ok := payload["serial_number"]; ok {
		_ = json.Unmarshal(v, &serial)
	}
	var deviceName string
	if v, ok := payload["device_name"]; ok {
		_ = json.Unmarshal(v, &deviceName)
	}
	c.mu.Lock()
	dev := c.devices[serial]
	if dev == nil {
		// Fallback to device_name match — pyeconet's "@SIGNAL" hack.
		for _, d := range c.devices {
			if d.DeviceID() == deviceName {
				dev = d
				break
			}
		}
	}
	c.mu.Unlock()
	if dev == nil {
		c.logger.Debug("rheemcloud: update for unknown device", "serial", serial, "device_name", deviceName)
		return
	}
	if dev.applyUpdate(payload) {
		c.broadcast(Event{Kind: EventStateChange, At: time.Now(), Device: dev})
	}
}

// publish sends a desired-state payload for a device. payload should
// contain "@..." datapoint keys. Blocks until the broker acks the
// publish, ctx expires, or the client is closed.
func (c *Client) publish(ctx context.Context, dev *Device, payload map[string]any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	mc := c.mqtt
	account := c.accountID
	c.mu.Unlock()
	if mc == nil {
		return ErrClosed
	}
	out := map[string]any{
		"transactionId": fmt.Sprintf("ANDROID_%s", time.Now().Format("2006-01-02T15:04:05")),
		"device_name":   dev.DeviceID(),
		"serial_number": dev.SerialNumber(),
	}
	maps.Copy(out, payload)
	body, err := json.Marshal(out)
	if err != nil {
		return err
	}
	topic := "user/" + account + "/device/desired"
	tok := mc.Publish(topic, 0, false, body)
	select {
	case <-tok.Done():
		return tok.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- Commands -----------------------------------------------------------

// SetMode changes the operating mode of a water heater. The unit must
// advertise the requested mode in its SupportedModes list (or accept
// the equivalent @ENABLED toggle for ModeOff). Blocks until the
// publish is acked by the broker, ctx expires, or the client is
// closed; observe Subscribe to confirm the unit reported the new
// state.
func (c *Client) SetMode(ctx context.Context, dev *Device, m Mode) error {
	if dev == nil {
		return ErrNoDevice
	}
	payload := map[string]any{}
	if dev.SupportsOnOff() {
		if m == ModeOff {
			payload["@ENABLED"] = 0
		} else {
			payload["@ENABLED"] = 1
		}
	}
	if dev.SupportsModes() {
		labels := dev.modeLabels()
		idx := -1
		for i, label := range labels {
			if modeFromEnumText(label) == m {
				idx = i
				break
			}
		}
		if idx >= 0 {
			payload["@MODE"] = idx
		} else if m != ModeOff {
			return ErrModeUnsupported
		}
	}
	if len(payload) == 0 {
		return ErrModeUnsupported
	}
	return c.publish(ctx, dev, payload)
}

// SetSetpoint sets the water-temperature setpoint. The value is checked
// against the unit's reported limits before sending.
func (c *Client) SetSetpoint(ctx context.Context, dev *Device, degF int) error {
	if dev == nil {
		return ErrNoDevice
	}
	lo, hi := dev.SetpointLimits()
	if lo != 0 || hi != 0 {
		if degF < lo || degF > hi {
			return fmt.Errorf("%w: %d not in [%d, %d]", ErrSetpointOutOfRange, degF, lo, hi)
		}
	}
	return c.publish(ctx, dev, map[string]any{"@SETPOINT": degF})
}

// SetAway toggles vacation/away mode on units that support it.
func (c *Client) SetAway(ctx context.Context, dev *Device, on bool) error {
	if dev == nil {
		return ErrNoDevice
	}
	if !dev.SupportsAway() {
		return fmt.Errorf("rheemcloud: unit does not support away mode")
	}
	return c.publish(ctx, dev, map[string]any{"@AWAY": on})
}

// --- Events --------------------------------------------------------------

// EventKind labels what happened.
type EventKind int

const (
	EventStateChange EventKind = iota
	EventConnected
	EventDisconnected
)

func (k EventKind) String() string {
	switch k {
	case EventStateChange:
		return "state-change"
	case EventConnected:
		return "connected"
	case EventDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

// Event is delivered on the channel returned by Subscribe. For
// EventStateChange, Device points at the device whose state changed.
type Event struct {
	At     time.Time
	Kind   EventKind
	Device *Device
}

// Subscribe returns a channel of events. The channel is buffered; if a
// consumer falls behind, oldest events are dropped (logged).
func (c *Client) Subscribe() <-chan Event {
	ch := make(chan Event, defaultSubscriberCap)
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.closed {
		close(ch)
		return ch
	}
	c.subscribers = append(c.subscribers, ch)
	return ch
}

func (c *Client) broadcast(ev Event) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for _, ch := range c.subscribers {
		select {
		case ch <- ev:
		default:
			c.logger.Warn("rheemcloud: subscriber buffer full; dropping event")
		}
	}
}
