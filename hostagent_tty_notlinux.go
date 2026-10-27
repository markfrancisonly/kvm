//go:build !linux

package kvm

import (
	"errors"
	"os"
)

func openRawTTY(string) (*os.File, error) {
	return nil, errors.New("the host agent serial port is only available on linux")
}
