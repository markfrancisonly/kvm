//go:build linux && arm

package kvm

import (
	"errors"
	"testing"
)

func TestScreenshotRespectsPausedVideo(t *testing.T) {
	videoPauseMu.Lock()
	videoPausedBy = &Session{}
	videoPauseMu.Unlock()
	defer func() {
		videoPauseMu.Lock()
		videoPausedBy = nil
		videoPauseMu.Unlock()
	}()

	// nativeInstance is not set here, so reaching the capture path would panic.
	if _, err := captureScreenshot(); !errors.Is(err, errVideoPaused) {
		t.Fatalf("err = %v, want errVideoPaused", err)
	}
}
