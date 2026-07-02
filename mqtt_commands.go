package kvm

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jetkvm/kvm/internal/hidrpc"
	"github.com/jetkvm/kvm/internal/keyboard"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// --- Command Subscriptions ---

func (m *MQTTManager) subscribeCommands() {
	commands := map[string]mqtt.MessageHandler{
		m.topic("dc_power", "set"):        m.handleDCPowerCommand,
		m.topic("dc_restore", "set"):      m.handleDCRestoreCommand,
		m.topic("atx_power_short", "set"): m.handleATXPowerShortCommand,
		m.topic("atx_power_long", "set"):  m.handleATXPowerLongCommand,
		m.topic("atx_reset", "set"):       m.handleATXResetCommand,
		m.topic("jiggler", "set"):         m.handleJigglerCommand,
		m.topic("reboot", "set"):          m.handleRebootCommand,
		m.topic("update", "install"):      m.handleUpdateInstallCommand,
		m.topic("virtual_media", "set"):   m.handleVirtualMediaCommand,
	}

	for topic, handler := range commands {
		if token := m.client.Subscribe(topic, 1, handler); token.Wait() && token.Error() != nil {
			mqttLogger.Error().Err(token.Error()).Str("topic", topic).Msg("failed to subscribe")
		}
	}

	// Keyboard input commands are subscribed at QoS 0, unlike the commands
	// above: with the persistent session (CleanSession false), a QoS 1
	// subscription makes the broker queue commands published while the device
	// is offline and replay them on reconnect, and keystrokes must never be
	// replayed into the attached host. Brokers that do not queue QoS 0 for
	// offline persistent sessions (the common default) avoid that entirely;
	// brokers that do queue QoS 0 are a residual risk noted in the PR.
	// Retained messages are dropped in the handlers.
	keyboardCommands := map[string]mqtt.MessageHandler{
		m.topic("keyboard_macro", "set"): m.handleKeyboardMacroCommand,
		m.topic("keyboard_text", "set"):  m.handleKeyboardTextCommand,
		m.topic("keyboard_keys", "set"):  m.handleKeyboardKeysCommand,
	}

	for topic, handler := range keyboardCommands {
		if token := m.client.Subscribe(topic, 0, handler); token.Wait() && token.Error() != nil {
			mqttLogger.Error().Err(token.Error()).Str("topic", topic).Msg("failed to subscribe")
		}
	}

	mqttLogger.Info().Msg("subscribed to command topics")
}

func (m *MQTTManager) handleDCPowerCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("DC power command rejected: actions are disabled")
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	mqttLogger.Info().Str("payload", payload).Msg("received DC power command")

	switch strings.ToUpper(payload) {
	case "ON":
		if err := setDCPowerState(true); err != nil {
			mqttLogger.Error().Err(err).Msg("failed to set DC power on")
		}
	case "OFF":
		if err := setDCPowerState(false); err != nil {
			mqttLogger.Error().Err(err).Msg("failed to set DC power off")
		}
	default:
		mqttLogger.Warn().Str("payload", payload).Msg("unknown DC power command")
	}
}

func (m *MQTTManager) handleDCRestoreCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("DC restore command rejected: actions are disabled")
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	mqttLogger.Info().Str("payload", payload).Msg("received DC restore command")

	var state int
	switch strings.ToLower(payload) {
	case "off":
		state = 0
	case "on":
		state = 1
	case "last_state":
		state = 2
	default:
		mqttLogger.Warn().Str("payload", payload).Msg("unknown DC restore command")
		return
	}

	if err := setDCRestoreState(state); err != nil {
		mqttLogger.Error().Err(err).Msg("failed to set DC restore state")
	}
}

func (m *MQTTManager) handleATXPowerShortCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("ATX power short command rejected: actions are disabled")
		return
	}
	mqttLogger.Info().Msg("received ATX power short press command")
	if err := pressATXPowerButton(500 * time.Millisecond); err != nil {
		mqttLogger.Error().Err(err).Msg("failed to press ATX power button (short)")
	}
}

func (m *MQTTManager) handleATXPowerLongCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("ATX power long command rejected: actions are disabled")
		return
	}
	mqttLogger.Info().Msg("received ATX power long press command")
	if err := pressATXPowerButton(5 * time.Second); err != nil {
		mqttLogger.Error().Err(err).Msg("failed to press ATX power button (long)")
	}
}

func (m *MQTTManager) handleATXResetCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("ATX reset command rejected: actions are disabled")
		return
	}
	mqttLogger.Info().Msg("received ATX reset command")
	if err := pressATXResetButton(500 * time.Millisecond); err != nil {
		mqttLogger.Error().Err(err).Msg("failed to press ATX reset button")
	}
}

func (m *MQTTManager) handleJigglerCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("jiggler command rejected: actions are disabled")
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	mqttLogger.Info().Str("payload", payload).Msg("received jiggler command")

	switch strings.ToUpper(payload) {
	case "ON":
		if err := rpcSetJigglerState(true); err != nil {
			mqttLogger.Error().Err(err).Msg("failed to enable jiggler")
		}
	case "OFF":
		if err := rpcSetJigglerState(false); err != nil {
			mqttLogger.Error().Err(err).Msg("failed to disable jiggler")
		}
	default:
		mqttLogger.Warn().Str("payload", payload).Msg("unknown jiggler command")
	}

	// Publish updated state immediately
	m.publishJigglerState()
}

func (m *MQTTManager) handleRebootCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("reboot command rejected: actions are disabled")
		return
	}
	mqttLogger.Info().Msg("received reboot command via MQTT")
	if err := rpcReboot(false); err != nil {
		mqttLogger.Error().Err(err).Msg("failed to reboot")
	}
}

func (m *MQTTManager) handleUpdateInstallCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("update install command rejected: actions are disabled")
		return
	}
	mqttLogger.Info().Msg("received update install command via MQTT")

	// Set flag to keep in_progress state until OTA state confirms updating
	m.updateRequested.Store(true)

	// Determine latest version for the in_progress message
	latestVer := lastKnownLatestVersion
	if latestVer == "" {
		latestVer = getInstalledVersion()
	}

	// Immediately publish in_progress state so HA shows the update dialog
	var zero float32
	m.publish(m.topic("update", "state"), mqttUpdateState{
		InstalledVersion: getInstalledVersion(),
		LatestVersion:    latestVer,
		InProgress:       true,
		UpdatePercentage: &zero,
	}, true)

	if err := rpcTryUpdate(); err != nil {
		mqttLogger.Error().Err(err).Msg("failed to start update")
		m.updateRequested.Store(false)
		// Reset in_progress on failure
		m.publishUpdateState()
	}
}

func (m *MQTTManager) handleVirtualMediaCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("virtual media command rejected: actions are disabled")
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	mqttLogger.Info().Str("payload", payload).Msg("received virtual media command")

	if payload == "-- no media --" {
		// Unmount current image
		if err := rpcUnmountImage(); err != nil {
			mqttLogger.Error().Err(err).Msg("failed to unmount image")
		}
	} else {
		// Defense-in-depth: reject obviously malicious filenames before
		// passing them to rpcMountWithStorage (which also validates via sanitizeFilename).
		if strings.Contains(payload, "..") || strings.ContainsRune(payload, '/') || strings.ContainsRune(payload, '\\') {
			mqttLogger.Warn().Str("payload", payload).Msg("rejected invalid filename")
			return
		}

		// Check if something is already mounted, unmount first
		virtualMediaStateMutex.RLock()
		mounted := currentVirtualMediaState != nil
		virtualMediaStateMutex.RUnlock()
		if mounted {
			if err := rpcUnmountImage(); err != nil {
				mqttLogger.Error().Err(err).Msg("failed to unmount current image before mounting new one")
				return
			}
		}

		// Mount the image as Disk mode (default).
		// rpcMountWithStorage validates the filename and checks file existence.
		if err := rpcMountWithStorage(payload, Disk); err != nil {
			mqttLogger.Error().Err(err).Msg("failed to mount image")
		}
	}

	// Publish updated state immediately
	m.publishVirtualMediaState()
}

const (
	// maxKeyboardTextLength bounds how much text a single keyboard text
	// command may type (each character takes ~40ms, so this is already ~40s
	// of typing).
	maxKeyboardTextLength = 1024
	// maxKeyboardKeysLength bounds a hotkey combo payload; a combo is a
	// handful of tokens, so anything larger is malformed.
	maxKeyboardKeysLength = 256
)

// findKeyboardMacro locates a stored keyboard macro by ID, or by name
// (case-insensitive) if no ID matches.
func findKeyboardMacro(nameOrID string) (*KeyboardMacro, bool) {
	for i := range config.KeyboardMacros {
		if config.KeyboardMacros[i].ID == nameOrID {
			return &config.KeyboardMacros[i], true
		}
	}
	for i := range config.KeyboardMacros {
		if strings.EqualFold(config.KeyboardMacros[i].Name, nameOrID) {
			return &config.KeyboardMacros[i], true
		}
	}
	return nil, false
}

// macroHIDSteps converts a stored macro's steps into wire-level HID steps.
func macroHIDSteps(steps []KeyboardMacroStep) ([]hidrpc.KeyboardMacroStep, error) {
	macroSteps := make([]keyboard.MacroStep, len(steps))
	for i, step := range steps {
		macroSteps[i] = keyboard.MacroStep(step)
	}
	return keyboard.HIDSteps(macroSteps)
}

// executeKeyboardSteps runs converted macro steps on a new goroutine — the
// steps sleep between reports, and paho delivers all messages on one ordered
// goroutine, so executing inline would stall every other command handler.
// rpcExecuteKeyboardMacro cancels any running macro and serializes execution,
// so overlapping commands run one after another rather than interleaving.
func executeKeyboardSteps(source string, steps []hidrpc.KeyboardMacroStep) {
	go func() {
		if err := rpcExecuteKeyboardMacro(steps); err != nil {
			if errors.Is(err, context.Canceled) {
				mqttLogger.Info().Str("source", source).Msg("keyboard input canceled")
				return
			}
			mqttLogger.Error().Err(err).Str("source", source).Msg("failed to execute keyboard input")
		}
	}()
}

func (m *MQTTManager) handleKeyboardMacroCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("keyboard macro command rejected: actions are disabled")
		return
	}
	if msg.Retained() {
		mqttLogger.Warn().Msg("ignoring retained keyboard macro command")
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	mqttLogger.Info().Str("payload", payload).Msg("received keyboard macro command")

	macro, ok := findKeyboardMacro(payload)
	if !ok {
		mqttLogger.Warn().Str("payload", payload).Msg("unknown keyboard macro")
		return
	}

	steps, err := macroHIDSteps(macro.Steps)
	if err != nil {
		mqttLogger.Error().Err(err).Str("macro", macro.Name).Msg("failed to convert keyboard macro")
		return
	}

	executeKeyboardSteps("macro "+macro.Name, steps)
}

func (m *MQTTManager) handleKeyboardTextCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("keyboard text command rejected: actions are disabled")
		return
	}
	if msg.Retained() {
		mqttLogger.Warn().Msg("ignoring retained keyboard text command")
		return
	}
	// The payload is typed verbatim and never logged: it may contain credentials.
	text := string(msg.Payload())
	mqttLogger.Info().Int("length", len(text)).Msg("received keyboard text command")

	if text == "" {
		mqttLogger.Warn().Msg("empty keyboard text command")
		return
	}
	if len(text) > maxKeyboardTextLength {
		mqttLogger.Warn().Int("length", len(text)).Int("max", maxKeyboardTextLength).Msg("keyboard text command too long")
		return
	}

	macroSteps, err := keyboard.StepsFromText(text, config.KeyboardLayout)
	if err != nil {
		mqttLogger.Warn().Err(err).Msg("failed to convert keyboard text")
		return
	}

	steps, err := keyboard.HIDSteps(macroSteps)
	if err != nil {
		mqttLogger.Error().Err(err).Msg("failed to encode keyboard text")
		return
	}

	executeKeyboardSteps("text", steps)

	// Reset the Home Assistant text field to empty so the same text can be sent
	// again (HA only re-publishes on a value change). Never echo the payload —
	// it may contain a password.
	m.publishString(m.topic("keyboard_text", "state"), "", true)
}

func (m *MQTTManager) handleKeyboardKeysCommand(client mqtt.Client, msg mqtt.Message) {
	if !m.actionsAllowed() {
		mqttLogger.Warn().Msg("keyboard keys command rejected: actions are disabled")
		return
	}
	if msg.Retained() {
		mqttLogger.Warn().Msg("ignoring retained keyboard keys command")
		return
	}
	payload := strings.TrimSpace(string(msg.Payload()))
	mqttLogger.Info().Str("payload", payload).Msg("received keyboard keys command")

	if len(payload) > maxKeyboardKeysLength {
		mqttLogger.Warn().Int("length", len(payload)).Int("max", maxKeyboardKeysLength).Msg("keyboard keys command too long")
		return
	}

	combo, err := keyboard.ParseCombo(payload)
	if err != nil {
		mqttLogger.Warn().Err(err).Msg("invalid key combo")
		return
	}

	steps, err := keyboard.HIDSteps([]keyboard.MacroStep{combo})
	if err != nil {
		mqttLogger.Error().Err(err).Str("payload", payload).Msg("failed to convert key combo")
		return
	}

	executeKeyboardSteps("keys "+payload, steps)
}
