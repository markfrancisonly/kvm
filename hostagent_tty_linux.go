//go:build linux

package kvm

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openRawTTY opens a tty with no echo, no line editing and no newline
// translation, so console bytes and protocol frames pass through unchanged.
// The termios ioctls go through SyscallConn: calling Fd() would switch the
// file to blocking mode and Close could no longer interrupt a pending Read.
func openRawTTY(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	conn, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
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
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("raw mode on %s: %w", path, err)
	}
	return f, nil
}
