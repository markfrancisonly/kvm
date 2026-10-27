package hostagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

// fakeAgent sits on the host end of two pipes.
type fakeAgent struct {
	t      *testing.T
	w      io.Writer // agent -> device
	frames chan Frame
}

type rw struct {
	io.Reader
	io.Writer
}

func newTestManager(t *testing.T) (*Manager, *fakeAgent, func()) {
	t.Helper()
	agentToDevR, agentToDevW := io.Pipe()
	devToAgentR, devToAgentW := io.Pipe()
	log := zerolog.Nop()
	m := New(&log, "0.0.0-test")
	m.AckTimeout = 300 * time.Millisecond
	m.IdleTimeout = 200 * time.Millisecond
	m.idleCheck = 20 * time.Millisecond

	a := &fakeAgent{t: t, w: agentToDevW, frames: make(chan Frame, 64)}
	go func() {
		var d Decoder
		buf := make([]byte, 4096)
		for {
			n, err := devToAgentR.Read(buf)
			if n > 0 {
				d.Feed(buf[:n])
				for {
					f, ok := d.Next()
					if !ok {
						break
					}
					a.frames <- f
				}
			}
			if err != nil {
				return
			}
		}
	}()
	attachDone := make(chan error, 1)
	go func() { attachDone <- m.Attach(rw{agentToDevR, devToAgentW}) }()

	cleanup := func() {
		agentToDevW.Close()
		agentToDevR.Close()
		devToAgentW.Close()
		devToAgentR.Close()
		select {
		case <-attachDone:
		case <-time.After(time.Second):
			t.Error("Attach did not return after the pipes closed")
		}
	}
	return m, a, cleanup
}

func (a *fakeAgent) send(f Frame) {
	if _, err := a.w.Write(f.Encode()); err != nil {
		a.t.Fatal(err)
	}
}

func (a *fakeAgent) expect(typ Type) Frame {
	a.t.Helper()
	select {
	case f := <-a.frames:
		if f.Type != typ {
			a.t.Fatalf("got %s frame, want %s", f.Type, typ)
		}
		return f
	case <-time.After(time.Second):
		a.t.Fatalf("no %s frame arrived", typ)
	}
	return Frame{}
}

func (a *fakeAgent) hello() {
	a.send(Frame{Type: TypeHello, Seq: 1, Payload: []byte(`{"v":1,"os":"windows","agent":"test","version":"1.2","caps":["text","image"]}`)})
	ack := a.expect(TypeAck)
	seq, note, ok := ParseAck(ack.Payload)
	if !ok || seq != 1 || !strings.Contains(note, `"fw":"0.0.0-test"`) {
		a.t.Fatalf("bad hello ack: %d %q %v", seq, note, ok)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHelloAnnouncesAgent(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()

	var mu sync.Mutex
	var seen []Status
	m.OnStatusChange(func(s Status) {
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
	})
	if m.Status().Present {
		t.Fatal("agent present before hello")
	}
	a.hello()
	s := m.Status()
	if !s.Present || s.OS != "windows" || s.Agent != "test" || s.Version != "1.2" || len(s.Caps) != 2 {
		t.Fatalf("status %+v", s)
	}
	waitFor(t, "status callback", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 1 && seen[0].Present
	})

	a.send(Frame{Type: TypePing, Seq: 2})
	a.expect(TypePong)

	a.send(Frame{Type: TypeBye, Seq: 3})
	waitFor(t, "bye", func() bool { return !m.Status().Present })
}

func TestHelloWithWrongVersionIsRefused(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()
	a.send(Frame{Type: TypeHello, Seq: 5, Payload: []byte(`{"v":2}`)})
	nack := a.expect(TypeNack)
	if seq, note, _ := ParseAck(nack.Payload); seq != 5 || note != "unsupported" {
		t.Fatalf("nack %d %q", seq, note)
	}
	if m.Status().Present {
		t.Fatal("agent accepted")
	}
}

func TestSendText(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()

	if err := m.SendText(context.Background(), "x", false); err != ErrNoAgent {
		t.Fatalf("without agent: %v", err)
	}
	a.hello()

	done := make(chan error, 1)
	go func() { done <- m.SendText(context.Background(), "héllo", true) }()
	f := a.expect(TypeText)
	if string(f.Payload) != "héllo" || f.Flags&FlagTextClipboard == 0 || f.Seq&SeqDeviceBit == 0 {
		t.Fatalf("text frame %+v", f)
	}
	a.send(Frame{Type: TypeAck, Seq: 9, Payload: AckPayload(f.Seq, "")})
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	go func() { done <- m.SendText(context.Background(), "nope", false) }()
	f = a.expect(TypeText)
	a.send(Frame{Type: TypeNack, Seq: 10, Payload: AckPayload(f.Seq, "elevated")})
	if err := <-done; err != ErrElevated {
		t.Fatalf("nack elevated: %v", err)
	}

	go func() { done <- m.SendText(context.Background(), "slow", false) }()
	a.expect(TypeText)
	if err := <-done; err != ErrTimeout {
		t.Fatalf("no ack: %v", err)
	}

	if err := m.SendText(context.Background(), strings.Repeat("a", MaxText+1), false); err != ErrTextTooLarge {
		t.Fatalf("oversize: %v", err)
	}
}

func TestSendTextSplitsLongText(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()
	a.hello()

	// Two-byte runes so a naive cut would split one.
	text := strings.Repeat("é", maxTextChunk)
	done := make(chan error, 1)
	go func() { done <- m.SendText(context.Background(), text, false) }()
	var got strings.Builder
	for i := 0; i < 2; i++ {
		f := a.expect(TypeText)
		if len(f.Payload) > maxTextChunk || !utf8.Valid(f.Payload) {
			t.Fatalf("chunk %d has %d bytes or splits a rune", i, len(f.Payload))
		}
		got.Write(f.Payload)
		a.send(Frame{Type: TypeAck, Seq: uint32(20 + i), Payload: AckPayload(f.Seq, "")})
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got.String() != text {
		t.Fatal("reassembled text differs")
	}
}

func TestConsoleTrafficAndAgentExclusivity(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()

	var mu sync.Mutex
	var console []byte
	m.SetConsoleSink(func(b []byte) {
		mu.Lock()
		console = append(console, b...)
		mu.Unlock()
	})
	if _, err := a.w.Write([]byte("login: ")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "console bytes", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return string(console) == "login: "
	})
	if err := m.WriteConsole([]byte("root\n")); err != nil {
		t.Fatal(err)
	}

	a.hello()
	if err := m.WriteConsole([]byte("x")); err != ErrConsoleBusy {
		t.Fatalf("console write while agent present: %v", err)
	}
	mu.Lock()
	before := len(console)
	mu.Unlock()
	a.send(Frame{Type: TypePing, Seq: 2})
	a.expect(TypePong)
	mu.Lock()
	after := len(console)
	mu.Unlock()
	if after != before {
		t.Fatal("agent frames leaked into the console")
	}
}

func TestIdleAgentReturnsPortToConsole(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()
	a.hello()
	waitFor(t, "idle timeout", func() bool { return !m.Status().Present })
	if err := m.SendText(context.Background(), "x", false); err != ErrNoAgent {
		t.Fatalf("after idle: %v", err)
	}
}

func TestRelayFromBrowser(t *testing.T) {
	m, a, cleanup := newTestManager(t)
	defer cleanup()

	browser := make(chan Frame, 16)
	id := m.SetBrowserSink(func(b []byte) {
		f, err := DecodeExact(b)
		if err != nil {
			t.Error(err)
			return
		}
		browser <- f
	})
	expectBrowser := func(typ Type, note string) Frame {
		t.Helper()
		select {
		case f := <-browser:
			if f.Type != typ {
				t.Fatalf("browser got %s, want %s", f.Type, typ)
			}
			if note != "" {
				if _, n, _ := ParseAck(f.Payload); n != note {
					t.Fatalf("browser note %q, want %q", n, note)
				}
			}
			return f
		case <-time.After(time.Second):
			t.Fatalf("browser got no %s frame", typ)
		}
		return Frame{}
	}

	begin := Frame{Type: TypeBlobBegin, Seq: 1, Payload: []byte(`{"id":7,"kind":"image","name":"shot.png","mime":"image/png","size":3,"paste":true}`)}
	if err := m.RelayFromBrowser(begin.Encode()); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("relay before hello: %v", err)
	}
	expectBrowser(TypeNack, "no_agent")

	a.hello()
	if err := m.RelayFromBrowser(begin.Encode()); err != nil {
		t.Fatal(err)
	}
	if f := a.expect(TypeBlobBegin); f.Seq != 1 {
		t.Fatalf("relayed seq %d", f.Seq)
	}
	a.send(Frame{Type: TypeAck, Seq: 50, Payload: AckPayload(1, "")})
	expectBrowser(TypeAck, "")

	if err := m.RelayFromBrowser(Frame{Type: TypeText, Seq: 2, Payload: []byte("x")}.Encode()); err == nil {
		t.Fatal("browser text frame was relayed")
	}
	expectBrowser(TypeNack, "unsupported")

	if err := m.RelayFromBrowser(Frame{Type: TypeBlobData, Seq: SeqDeviceBit | 3}.Encode()); err == nil {
		t.Fatal("device sequence bit accepted from the browser")
	}
	expectBrowser(TypeNack, "bad_seq")

	if err := m.RelayFromBrowser([]byte("garbage")); err == nil {
		t.Fatal("garbage relayed")
	}
	expectBrowser(TypeNack, "bad_frame")

	a.send(Frame{Type: TypeStatus, Seq: 51, Payload: []byte(`{"foreground":"elevated"}`)})
	expectBrowser(TypeStatus, "")

	// Closing the browser channel mid-transfer cancels the blob at the agent.
	m.ClearBrowserSink(id)
	cancel := a.expect(TypeBlobCancel)
	if cancel.Seq&SeqDeviceBit == 0 {
		t.Fatal("cancel is not a device frame")
	}
	if seq, _, _ := ParseAck(cancel.Payload); seq != 7 {
		t.Fatalf("cancelled blob %d, want 7", seq)
	}
}

func TestAttachTwiceIsRefused(t *testing.T) {
	m, _, cleanup := newTestManager(t)
	defer cleanup()
	waitFor(t, "attach", m.Attached)
	if err := m.Attach(rw{strings.NewReader(""), io.Discard}); err != ErrAttached {
		t.Fatalf("second attach: %v", err)
	}
}
