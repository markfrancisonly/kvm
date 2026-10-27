package kvm

import (
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

const (
	cdcACMDevicePath       = "/dev/ttyGS0"
	hostAgentConsoleNotice = "\r\n[JetKVM] The host agent is using this USB serial port; console input is paused until it exits.\r\n"
)

// handleCDCACMChannel serves the USB serial console over a data channel. The
// tty itself is held open by the host agent supervisor (hostagent.go), which
// passes console bytes through while no agent has announced itself.
func handleCDCACMChannel(d *webrtc.DataChannel) {
	scopedLogger := cdcACMLogger.With().
		Uint16("data_channel_id", *d.ID()).Logger()
	var sinkID atomic.Uint64

	d.OnOpen(func() {
		sinkID.Store(hostAgent.SetConsoleSink(func(b []byte) {
			if err := d.Send(b); err != nil {
				scopedLogger.Warn().Err(err).Msg("Failed to send CDC-ACM output")
			}
		}))
		if hostAgent.Status().Present {
			if err := d.Send([]byte(hostAgentConsoleNotice)); err != nil {
				scopedLogger.Warn().Err(err).Msg("Failed to send the host agent notice")
			}
		}
		scopedLogger.Info().Msg("CDC-ACM console channel opened")
	})

	d.OnMessage(func(msg webrtc.DataChannelMessage) {
		if err := hostAgent.WriteConsole(msg.Data); err != nil {
			scopedLogger.Debug().Err(err).Msg("CDC-ACM console input dropped")
		}
	})

	d.OnClose(func() {
		hostAgent.ClearConsoleSink(sinkID.Load())
		scopedLogger.Info().Msg("CDC-ACM console channel closed")
	})

	d.OnError(func(err error) {
		scopedLogger.Warn().Err(err).Msg("CDC-ACM console channel error")
	})
}
