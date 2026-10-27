package kvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/jetkvm/kvm/internal/hostagent"
)

// The host agent shares the USB serial console port (the CDC-ACM gadget
// function). While the function is enabled the device keeps the tty open and
// hands its bytes either to the console channel or, once an agent has
// announced itself, to the protocol in internal/hostagent.

var (
	hostAgent        *hostagent.Manager
	hostAgentEnabled atomic.Bool
	hostAgentWake    = make(chan struct{}, 1)
)

const hostAgentRetryInterval = 2 * time.Second

func initHostAgent() {
	hostAgent = hostagent.New(hostAgentLogger, builtAppVersion)
	hostAgent.OnStatusChange(func(s hostagent.Status) {
		if currentSession != nil {
			writeJSONRPCEvent("hostAgentState", s, currentSession)
		}
	})
	hostAgent.SetEnabled(config.HostAgentEnabled)
	hostAgentApplyConfig()
	go hostAgentSupervisor()
}

// hostAgentApplyConfig follows the USB serial console toggle.
func hostAgentApplyConfig() {
	enabled := config != nil && config.UsbDevices != nil && config.UsbDevices.SerialConsole
	hostAgentEnabled.Store(enabled)
	select {
	case hostAgentWake <- struct{}{}:
	default:
	}
}

// hostAgentSupervisor keeps the gadget tty attached to the manager while the
// serial console function is enabled. The device node disappears while the
// gadget is reconfigured, so the port is reopened after every error.
func hostAgentSupervisor() {
	for {
		if !hostAgentEnabled.Load() {
			<-hostAgentWake
			continue
		}
		f, err := openRawTTY(cdcACMDevicePath)
		if err != nil {
			hostAgentLogger.Debug().Err(err).Str("path", cdcACMDevicePath).Msg("USB serial port not available")
			hostAgentSleep()
			continue
		}
		done := make(chan error, 1)
		go func() { done <- hostAgent.Attach(f) }()
		hostAgentLogger.Info().Str("path", cdcACMDevicePath).Msg("serving the USB serial port")

		attached := true
		for attached {
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrClosed) {
					hostAgentLogger.Warn().Err(err).Msg("USB serial port read ended")
				}
				_ = f.Close()
				hostAgentSleep()
				attached = false
			case <-hostAgentWake:
				if hostAgentEnabled.Load() {
					continue
				}
				_ = f.Close()
				<-done
				attached = false
			}
		}
	}
}

func hostAgentSleep() {
	select {
	case <-hostAgentWake:
	case <-time.After(hostAgentRetryInterval):
	}
}

func reportHostAgentState(session *Session) {
	writeJSONRPCEvent("hostAgentState", hostAgent.Status(), session)
}

func rpcGetHostAgentState() (hostagent.Status, error) {
	return hostAgent.Status(), nil
}

// rpcSetHostAgentEnabled is the device setting that allows a host agent on
// the USB serial port at all. Off by default.
func rpcSetHostAgentEnabled(enabled bool) (bool, error) {
	config.HostAgentEnabled = enabled
	if err := SaveConfig(); err != nil {
		return config.HostAgentEnabled, fmt.Errorf("failed to save config: %w", err)
	}
	hostAgent.SetEnabled(enabled)
	return enabled, nil
}

// rpcPasteText hands text to the host agent. By default the agent puts the
// text on the target's clipboard and presses Ctrl+V, which is what a paste
// means to the focused application; mode "type" sends Unicode key events
// instead, for applications that do not accept a paste. The error data is a
// short code (disabled, no_agent, elevated, timeout, too_large) so the UI can
// fall back to keystrokes.
func rpcPasteText(text string, mode string) error {
	clipboard := true
	switch mode {
	case "", "clipboard":
	case "type":
		clipboard = false
	default:
		return fmt.Errorf("invalid mode: %s", mode)
	}
	return hostAgent.SendText(context.Background(), text, clipboard)
}

// handleHostAgentChannel relays blob frames between the browser and the agent.
func handleHostAgentChannel(d *webrtc.DataChannel) {
	scopedLogger := hostAgentLogger.With().
		Uint16("data_channel_id", *d.ID()).Logger()
	var sinkID atomic.Uint64

	d.OnOpen(func() {
		sinkID.Store(hostAgent.SetBrowserSink(func(b []byte) {
			if err := d.Send(b); err != nil {
				scopedLogger.Warn().Err(err).Msg("Failed to send a host agent frame to the browser")
			}
		}))
		scopedLogger.Info().Msg("host agent channel opened")
	})

	d.OnMessage(func(msg webrtc.DataChannelMessage) {
		if err := hostAgent.RelayFromBrowser(msg.Data); err != nil {
			scopedLogger.Debug().Err(err).Msg("blob frame not relayed")
		}
	})

	d.OnClose(func() {
		hostAgent.ClearBrowserSink(sinkID.Load())
		scopedLogger.Info().Msg("host agent channel closed")
	})

	d.OnError(func(err error) {
		scopedLogger.Warn().Err(err).Msg("host agent channel error")
	})
}
