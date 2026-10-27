package hostagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

// State says who is on the other end of the USB serial port.
type State int

const (
	// StateConsole is the default: bytes belong to the USB serial console.
	StateConsole State = iota
	// StateAgent is entered when the host agent has announced itself; the port
	// then carries protocol frames only.
	StateAgent
)

// Hello is the agent's announcement payload.
type Hello struct {
	Version      int      `json:"v"`
	OS           string   `json:"os"`
	Agent        string   `json:"agent"`
	AgentVersion string   `json:"version"`
	Caps         []string `json:"caps"`
}

// Status is reported to the UI as the hostAgentState event.
type Status struct {
	// Enabled is the device setting; while it is off no agent is accepted.
	Enabled bool     `json:"enabled"`
	Present bool     `json:"present"`
	OS      string   `json:"os,omitempty"`
	Agent   string   `json:"agent,omitempty"`
	Version string   `json:"version,omitempty"`
	Caps    []string `json:"caps,omitempty"`
}

// Errors are short codes because the UI matches on them to pick a fallback.
var (
	ErrDisabled = errors.New("disabled")
	ErrNoAgent  = errors.New("no_agent")
	// ErrPartial: a frame was sent but its outcome is unknown (no answer, or
	// a later frame failed after earlier ones were delivered), so the caller
	// must not send the text again.
	ErrPartial      = errors.New("partial")
	ErrElevated     = errors.New("elevated")
	ErrTextTooLarge = errors.New("too_large")
	ErrConsoleBusy  = errors.New("console_busy")
	ErrAttached     = errors.New("hostagent: already attached")
)

const (
	// MaxText bounds one pasteText call. It is split into frames below.
	MaxText      = 1024 * 1024
	maxTextChunk = 60 * 1024

	defaultIdleTimeout = 15 * time.Second
	defaultAckTimeout  = 10 * time.Second
	defaultIdleCheck   = time.Second
)

type ackResult struct {
	ok   bool
	note string
}

// Manager runs the protocol over whatever is attached, normally the gadget
// serial tty. It is safe for concurrent use.
type Manager struct {
	log        *zerolog.Logger
	deviceInfo []byte

	// IdleTimeout returns the port to the console when the agent goes quiet.
	IdleTimeout time.Duration
	// AckTimeout bounds the wait for the agent's answer to a text frame.
	AckTimeout time.Duration
	idleCheck  time.Duration

	mu          sync.Mutex
	enabled     bool
	link        io.Writer
	state       State
	info        Hello
	lastFrame   time.Time
	consoleSink func([]byte)
	consoleID   uint64
	browserSink func([]byte)
	browserID   uint64
	onStatus    func(Status)
	seq         uint32
	waiters     map[uint32]chan ackResult
	activeBlob  *uint32
	nextSinkID  uint64

	writeMu sync.Mutex
	textMu  sync.Mutex
	dec     Decoder
}

// New returns a manager. version is the app version the agent sees in the
// answer to its announcement.
func New(log *zerolog.Logger, version string) *Manager {
	info, _ := json.Marshal(map[string]any{"v": Version, "device": "jetkvm", "fw": version})
	return &Manager{
		log:         log,
		deviceInfo:  info,
		IdleTimeout: defaultIdleTimeout,
		AckTimeout:  defaultAckTimeout,
		idleCheck:   defaultIdleCheck,
		waiters:     map[uint32]chan ackResult{},
	}
}

// Attach serves rw until a read fails and returns that error. Close rw from
// another goroutine to stop it. Only one attachment at a time is allowed.
func (m *Manager) Attach(rw io.ReadWriter) error {
	m.mu.Lock()
	if m.link != nil {
		m.mu.Unlock()
		return ErrAttached
	}
	m.link = rw
	m.lastFrame = time.Now()
	m.mu.Unlock()
	m.dec.Reset()

	stop := make(chan struct{})
	go m.idleLoop(stop)

	buf := make([]byte, 4096)
	var err error
	for {
		n, rerr := rw.Read(buf)
		if n > 0 {
			m.handleBytes(buf[:n])
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	close(stop)

	m.mu.Lock()
	m.link = nil
	changed := m.setStateLocked(StateConsole)
	for seq, ch := range m.waiters {
		ch <- ackResult{note: "detached"}
		delete(m.waiters, seq)
	}
	m.activeBlob = nil
	m.mu.Unlock()
	if changed != nil {
		changed()
	}
	return err
}

// Attached reports whether a port is being served.
func (m *Manager) Attached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.link != nil
}

// Status describes the agent for the UI.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// SetEnabled applies the device setting. Disabling drops a connected agent.
// New managers start disabled.
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	if m.enabled == enabled {
		m.mu.Unlock()
		return
	}
	m.enabled = enabled
	if !enabled && m.state == StateAgent {
		m.activeBlob = nil
		changed := m.setStateLocked(StateConsole)
		m.mu.Unlock()
		if changed != nil {
			changed()
		}
		return
	}
	fn, status := m.onStatus, m.statusLocked()
	m.mu.Unlock()
	if fn != nil {
		fn(status)
	}
}

func (m *Manager) statusLocked() Status {
	if m.state != StateAgent {
		return Status{Enabled: m.enabled}
	}
	return Status{
		Enabled: m.enabled,
		Present: true,
		OS:      m.info.OS,
		Agent:   m.info.Agent,
		Version: m.info.AgentVersion,
		Caps:    append([]string(nil), m.info.Caps...),
	}
}

// OnStatusChange registers the callback run whenever the agent appears or
// disappears. It is called without locks held.
func (m *Manager) OnStatusChange(fn func(Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStatus = fn
}

// setStateLocked changes the state and returns the status callback to run
// after unlocking, or nil when nothing changed.
func (m *Manager) setStateLocked(s State) func() {
	if m.state == s {
		return nil
	}
	m.state = s
	if s == StateConsole {
		m.info = Hello{}
	}
	fn, status := m.onStatus, m.statusLocked()
	m.log.Info().Bool("present", status.Present).Str("os", status.OS).Str("agent", status.Agent).
		Str("version", status.Version).Msg("host agent state changed")
	if fn == nil {
		return nil
	}
	return func() { fn(status) }
}

// SetConsoleSink receives bytes from the host while the port is in console
// state. The returned id is passed to ClearConsoleSink.
func (m *Manager) SetConsoleSink(fn func([]byte)) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSinkID++
	m.consoleSink, m.consoleID = fn, m.nextSinkID
	return m.consoleID
}

// ClearConsoleSink removes the sink if it is still the current one.
func (m *Manager) ClearConsoleSink(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.consoleID == id {
		m.consoleSink = nil
	}
}

// WriteConsole sends console input to the host. It is refused while the
// agent holds the port.
func (m *Manager) WriteConsole(b []byte) error {
	m.mu.Lock()
	link, state := m.link, m.state
	m.mu.Unlock()
	if link == nil {
		return io.ErrClosedPipe
	}
	if state == StateAgent {
		return ErrConsoleBusy
	}
	return m.writeRaw(b)
}

// SetBrowserSink receives frames addressed to the browser: acknowledgements
// of relayed blob frames and agent status. The returned id is passed to
// ClearBrowserSink.
func (m *Manager) SetBrowserSink(fn func([]byte)) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSinkID++
	m.browserSink, m.browserID = fn, m.nextSinkID
	return m.browserID
}

// ClearBrowserSink removes the sink if it is still the current one and cancels
// a blob transfer that sink started.
func (m *Manager) ClearBrowserSink(id uint64) {
	m.mu.Lock()
	if m.browserID != id {
		m.mu.Unlock()
		return
	}
	m.browserSink = nil
	blob := m.activeBlob
	m.activeBlob = nil
	state := m.state
	m.mu.Unlock()
	if blob != nil && state == StateAgent {
		if err := m.send(Frame{Type: TypeBlobCancel, Payload: AckPayload(*blob, "")}); err != nil {
			m.log.Warn().Err(err).Msg("failed to cancel the blob of a closed browser channel")
		}
	}
}

// RelayFromBrowser validates one blob frame from the browser and writes it to
// the agent. On refusal a NACK with the reason goes back to the browser.
func (m *Manager) RelayFromBrowser(b []byte) error {
	f, err := DecodeExact(b)
	if err != nil {
		m.nackBrowser(0, "bad_frame")
		return err
	}
	if f.Seq&SeqDeviceBit != 0 {
		m.nackBrowser(f.Seq, "bad_seq")
		return errors.New("hostagent: browser frame uses a device sequence number")
	}
	switch f.Type {
	case TypeBlobBegin, TypeBlobData, TypeBlobEnd, TypeBlobCancel:
	default:
		m.nackBrowser(f.Seq, "unsupported")
		return fmt.Errorf("hostagent: browser may not send %s frames", f.Type)
	}

	m.mu.Lock()
	if !m.enabled || m.state != StateAgent {
		err := ErrNoAgent
		if !m.enabled {
			err = ErrDisabled
		}
		m.mu.Unlock()
		m.nackBrowser(f.Seq, err.Error())
		return err
	}
	switch f.Type {
	case TypeBlobBegin:
		var begin struct {
			ID uint32 `json:"id"`
		}
		if err := json.Unmarshal(f.Payload, &begin); err != nil {
			m.mu.Unlock()
			m.nackBrowser(f.Seq, "bad_frame")
			return fmt.Errorf("hostagent: blob_begin payload: %w", err)
		}
		id := begin.ID
		m.activeBlob = &id
	case TypeBlobEnd, TypeBlobCancel:
		m.activeBlob = nil
	}
	m.mu.Unlock()
	return m.writeRaw(b)
}

func (m *Manager) nackBrowser(seq uint32, reason string) {
	m.toBrowser(Frame{Type: TypeNack, Seq: m.nextSeq(), Payload: AckPayload(seq, reason)})
}

func (m *Manager) toBrowser(f Frame) {
	m.mu.Lock()
	sink := m.browserSink
	m.mu.Unlock()
	if sink != nil {
		sink(f.Encode())
	}
}

// SendText asks the agent to paste text. clipboard selects the clipboard
// mode; otherwise the agent types the text. It returns ErrNoAgent when no
// agent holds the port and ErrElevated when the agent cannot reach the
// focused window.
func (m *Manager) SendText(ctx context.Context, text string, clipboard bool) error {
	if len(text) > MaxText {
		return ErrTextTooLarge
	}
	var flags uint8
	if clipboard {
		flags |= FlagTextClipboard
	}
	m.textMu.Lock()
	defer m.textMu.Unlock()
	delivered := false
	for len(text) > 0 {
		chunk := text
		if len(chunk) > maxTextChunk {
			cut := maxTextChunk
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			chunk = text[:cut]
		}
		text = text[len(chunk):]
		if err := m.request(ctx, Frame{Type: TypeText, Flags: flags, Payload: []byte(chunk)}); err != nil {
			if delivered {
				m.log.Warn().Err(err).Msg("text paste failed after part of it was delivered")
				return ErrPartial
			}
			return err
		}
		delivered = true
	}
	return nil
}

// request sends a device-originated frame and waits for its ACK or NACK.
func (m *Manager) request(ctx context.Context, f Frame) error {
	m.mu.Lock()
	if !m.enabled {
		m.mu.Unlock()
		return ErrDisabled
	}
	if m.state != StateAgent {
		m.mu.Unlock()
		return ErrNoAgent
	}
	f.Seq = m.nextSeqLocked()
	ch := make(chan ackResult, 1)
	m.waiters[f.Seq] = ch
	m.mu.Unlock()

	drop := func() {
		m.mu.Lock()
		delete(m.waiters, f.Seq)
		m.mu.Unlock()
	}
	if err := m.send(f); err != nil {
		drop()
		return err
	}
	timer := time.NewTimer(m.AckTimeout)
	defer timer.Stop()
	// The frame is out. Only an explicit refusal by the agent means nothing
	// was delivered; silence or a lost link leaves the outcome unknown.
	select {
	case r := <-ch:
		if r.ok {
			return nil
		}
		switch r.note {
		case ErrElevated.Error():
			return ErrElevated
		case "detached":
			return ErrPartial
		}
		return fmt.Errorf("host agent refused: %s", r.note)
	case <-ctx.Done():
		drop()
		return ErrPartial
	case <-timer.C:
		drop()
		return ErrPartial
	}
}

func (m *Manager) nextSeq() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextSeqLocked()
}

func (m *Manager) nextSeqLocked() uint32 {
	m.seq++
	return m.seq | SeqDeviceBit
}

// send writes a frame the device originates.
func (m *Manager) send(f Frame) error {
	if f.Seq == 0 {
		f.Seq = m.nextSeq()
	}
	return m.writeRaw(f.Encode())
}

func (m *Manager) writeRaw(b []byte) error {
	m.mu.Lock()
	link := m.link
	m.mu.Unlock()
	if link == nil {
		return io.ErrClosedPipe
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	for len(b) > 0 {
		n, err := link.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (m *Manager) idleLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(m.idleCheck)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.mu.Lock()
			var changed func()
			if m.state == StateAgent && time.Since(m.lastFrame) > m.IdleTimeout {
				m.log.Warn().Msg("host agent went quiet, returning the port to the console")
				changed = m.setStateLocked(StateConsole)
				m.activeBlob = nil
			}
			m.mu.Unlock()
			if changed != nil {
				changed()
			}
		}
	}
}

func (m *Manager) handleBytes(b []byte) {
	m.mu.Lock()
	state, sink := m.state, m.consoleSink
	m.mu.Unlock()
	if state == StateConsole && sink != nil {
		sink(b)
	}
	m.dec.Feed(b)
	for {
		f, ok := m.dec.Next()
		if !ok {
			return
		}
		m.handleFrame(f)
	}
}

func (m *Manager) handleFrame(f Frame) {
	m.mu.Lock()
	m.lastFrame = time.Now()
	state := m.state
	m.mu.Unlock()

	if f.Type == TypeHello {
		m.handleHello(f)
		return
	}
	if state != StateAgent {
		m.log.Debug().Str("type", f.Type.String()).Msg("frame ignored before the agent announced itself")
		return
	}
	switch f.Type {
	case TypePing:
		if err := m.send(Frame{Type: TypePong, Payload: AckPayload(f.Seq, "")}); err != nil {
			m.log.Warn().Err(err).Msg("failed to answer the agent's ping")
		}
	case TypePong:
	case TypeBye:
		m.mu.Lock()
		changed := m.setStateLocked(StateConsole)
		m.activeBlob = nil
		m.mu.Unlock()
		if changed != nil {
			changed()
		}
	case TypeAck, TypeNack:
		seq, note, ok := ParseAck(f.Payload)
		if !ok {
			m.log.Warn().Msg("agent acknowledgement without a sequence number")
			return
		}
		if seq&SeqDeviceBit == 0 {
			m.toBrowser(f)
			return
		}
		m.mu.Lock()
		ch := m.waiters[seq]
		delete(m.waiters, seq)
		m.mu.Unlock()
		if ch != nil {
			ch <- ackResult{ok: f.Type == TypeAck, note: note}
		}
	case TypeStatus, TypeBlobCancel:
		m.toBrowser(f)
	default:
		m.log.Debug().Str("type", f.Type.String()).Msg("unexpected frame from the agent")
	}
}

func (m *Manager) handleHello(f Frame) {
	m.mu.Lock()
	enabled := m.enabled
	m.mu.Unlock()
	if !enabled {
		m.log.Debug().Msg("a host agent announced itself while the setting is off")
		if err := m.send(Frame{Type: TypeNack, Payload: AckPayload(f.Seq, ErrDisabled.Error())}); err != nil {
			m.log.Warn().Err(err).Msg("failed to refuse the announcement")
		}
		return
	}
	var hello Hello
	if err := json.Unmarshal(f.Payload, &hello); err != nil || hello.Version != Version {
		m.log.Warn().Err(err).Int("version", hello.Version).Msg("rejecting the host agent announcement")
		if err := m.send(Frame{Type: TypeNack, Payload: AckPayload(f.Seq, "unsupported")}); err != nil {
			m.log.Warn().Err(err).Msg("failed to refuse the announcement")
		}
		return
	}
	m.mu.Lock()
	m.info = hello
	changed := m.setStateLocked(StateAgent)
	m.mu.Unlock()
	if changed != nil {
		changed()
	}
	if err := m.send(Frame{Type: TypeAck, Payload: AckPayload(f.Seq, string(m.deviceInfo))}); err != nil {
		m.log.Warn().Err(err).Msg("failed to acknowledge the announcement")
	}
}
