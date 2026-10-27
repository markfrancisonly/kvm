// hostagent-loopback exercises a host agent without hardware. It opens a
// pseudo terminal, serves its master side with the device-side manager, starts
// the agent on the slave side and drives text and blob transfers through it.
//
//	go run ./cmd/hostagent-loopback -- pwsh -File jetkvm-host-agent.ps1 -DevicePath {tty}
//
// {tty} is replaced with the slave path. Exit status 0 means every step was
// acknowledged.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/rs/zerolog"
	"golang.org/x/sys/unix"

	"github.com/jetkvm/kvm/internal/hostagent"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "loopback failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("usage: hostagent-loopback -- <agent command with {tty}>")
	}

	master, slave, err := pty.Open()
	if err != nil {
		return err
	}
	defer master.Close()
	for _, f := range []*os.File{master, slave} {
		if err := makeRaw(f); err != nil {
			return err
		}
	}

	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
	m := hostagent.New(&log, "loopback")
	m.OnStatusChange(func(s hostagent.Status) {
		log.Info().Bool("present", s.Present).Str("os", s.OS).Str("agent", s.Agent).Msg("agent status")
	})
	go func() {
		if err := m.Attach(master); err != nil && !errors.Is(err, os.ErrClosed) {
			log.Warn().Err(err).Msg("attach ended")
		}
	}()

	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "{tty}", slave.Name())
	}
	cmd := exec.Command(args[0], args[1:]...)
	// The agent echoes every text it would have typed; keep that readable.
	cmd.Stdout = &lineTruncater{w: os.Stderr}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// The agent opens the slave by path; this descriptor keeps the pty alive
	// until it does, otherwise the master read fails with EIO.
	defer slave.Close()

	if err := waitFor(20*time.Second, func() bool { return m.Status().Present }); err != nil {
		return fmt.Errorf("agent never announced itself: %w", err)
	}
	status := m.Status()
	log.Info().Strs("caps", status.Caps).Str("version", status.Version).Msg("agent present")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, text := range []string{"hello from the loopback\n", "ünïcödé ✓ 日本語 🎉\ttab"} {
		if err := m.SendText(ctx, text, false); err != nil {
			return fmt.Errorf("text %q: %w", text, err)
		}
		log.Info().Int("bytes", len(text)).Msg("text acknowledged")
	}
	if err := m.SendText(ctx, "clipboard mode", true); err != nil {
		return fmt.Errorf("clipboard text: %w", err)
	}
	if err := m.SendText(ctx, strings.Repeat("long ", 20000), false); err != nil {
		return fmt.Errorf("long text: %w", err)
	}
	log.Info().Msg("long text acknowledged in several frames")

	if err := blobRoundTrip(m, log); err != nil {
		return err
	}

	// The agent's ping keeps the state alive; a quiet period must not drop it.
	time.Sleep(6 * time.Second)
	if !m.Status().Present {
		return errors.New("agent dropped out during an idle period")
	}
	log.Info().Msg("loopback passed")
	return nil
}

// blobRoundTrip plays the browser: it relays a small file through the manager
// and expects the agent to acknowledge every frame.
func blobRoundTrip(m *hostagent.Manager, log zerolog.Logger) error {
	acks := make(chan hostagent.Frame, 16)
	id := m.SetBrowserSink(func(b []byte) {
		f, err := hostagent.DecodeExact(b)
		if err != nil {
			log.Warn().Err(err).Msg("bad frame for the browser")
			return
		}
		acks <- f
	})
	defer m.ClearBrowserSink(id)

	content := []byte(strings.Repeat("0123456789abcdef", 3000)) // 48000 bytes, three chunks
	meta, _ := json.Marshal(map[string]any{"id": 1, "kind": "file", "name": "loopback.txt", "mime": "text/plain", "size": len(content), "paste": false})
	var seq uint32 = 100
	expectAck := func(want uint32) error {
		select {
		case f := <-acks:
			got, note, _ := hostagent.ParseAck(f.Payload)
			if f.Type != hostagent.TypeAck || got != want {
				return fmt.Errorf("blob frame %d: got %s %d %q", want, f.Type, got, note)
			}
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("blob frame %d not acknowledged", want)
		}
	}
	relay := func(f hostagent.Frame) error {
		seq++
		f.Seq = seq
		if err := m.RelayFromBrowser(f.Encode()); err != nil {
			return err
		}
		return expectAck(seq)
	}
	if err := relay(hostagent.Frame{Type: hostagent.TypeBlobBegin, Payload: meta}); err != nil {
		return err
	}
	const chunk = 16 * 1024
	for off := 0; off < len(content); off += chunk {
		end := min(off+chunk, len(content))
		payload := make([]byte, 8+end-off)
		payload[0] = 1
		payload[4], payload[5], payload[6], payload[7] = byte(off), byte(off>>8), byte(off>>16), byte(off>>24)
		copy(payload[8:], content[off:end])
		if err := relay(hostagent.Frame{Type: hostagent.TypeBlobData, Payload: payload}); err != nil {
			return err
		}
	}
	if err := relay(hostagent.Frame{Type: hostagent.TypeBlobEnd, Payload: []byte{1, 0, 0, 0}}); err != nil {
		return err
	}
	log.Info().Int("bytes", len(content)).Msg("blob acknowledged")
	return nil
}

// lineTruncater shortens long output lines.
type lineTruncater struct {
	w   *os.File
	buf []byte
}

func (t *lineTruncater) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := t.buf[:i]
		if len(line) > 160 {
			line = append(append([]byte{}, line[:160]...), []byte(fmt.Sprintf("... (%d bytes)", len(line)))...)
		}
		_, _ = t.w.Write(append(line, '\n'))
		t.buf = t.buf[i+1:]
	}
}

func waitFor(d time.Duration, cond func() bool) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timeout")
}

func makeRaw(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	err = conn.Control(func(fd uintptr) {
		t, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
		if err != nil {
			ioctlErr = err
			return
		}
		t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
		t.Oflag &^= unix.OPOST
		t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		t.Cflag &^= unix.CSIZE | unix.PARENB
		t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
		t.Cc[unix.VMIN] = 1
		t.Cc[unix.VTIME] = 0
		ioctlErr = unix.IoctlSetTermios(int(fd), unix.TCSETS, t)
	})
	if err == nil {
		err = ioctlErr
	}
	return err
}
