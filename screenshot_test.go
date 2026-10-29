//go:build linux && arm

package kvm

import (
	"errors"
	"testing"

	"github.com/jetkvm/kvm/internal/native"
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

// pauseCheckingNative records whether the pause lock was held when capture
// was started.
type pauseCheckingNative struct {
	native.EmptyNativeInterface
	startedUnlocked bool
}

func (n *pauseCheckingNative) VideoStart() error {
	if videoPauseMu.TryLock() {
		n.startedUnlocked = true
		videoPauseMu.Unlock()
	}
	return nil
}

func (n *pauseCheckingNative) VideoGetSnapshot() ([]byte, error) { return []byte("jpeg"), nil }

func TestScreenshotStartsCaptureUnderPauseLock(t *testing.T) {
	fake := &pauseCheckingNative{}
	original := nativeInstance
	nativeInstance = fake
	defer func() { nativeInstance = original }()

	if _, err := captureScreenshot(); err != nil {
		t.Fatal(err)
	}
	if fake.startedUnlocked {
		t.Fatal("capture was started without the pause lock, so a pause could slip in and be undone")
	}
}
