// Package rheemcloud is a Go client for the Rheem EcoNet cloud service —
// the same backend the EcoNet mobile app talks to.
//
// Auth and the initial device list go over HTTPS to rheem.clearblade.com
// (a ClearBlade-hosted Backend-as-a-Service). State updates and commands
// go over MQTT/TLS to the same host on port 1884. There is no Rheem-
// published API; this package follows the protocol the open-source
// pyeconet library reverse-engineered from the Android app.
//
// You will need a Rheem EcoNet account (the one used by the iOS/Android
// app) and the heater's Wi-Fi module connected to your network. All
// commands traverse Rheem's servers; this is not a local-only client.
// For local control without internet, see the sibling econet-go package
// (which targets the esphome-econet RS485 bridge instead).
//
// The Client is long-lived. After Connect returns it maintains the
// MQTT session in the background and reconnects on drop. The context
// passed to Connect bounds the session setup only — REST auth, REST
// device list, and MQTT handshake — not the lifetime of the Client.
// Call Close to shut down the background session.
//
// Methods that touch the network (commands, usage queries, Refresh,
// DynamicAction) each accept their own context.Context for
// per-operation timeouts and cancellation.
package rheemcloud
