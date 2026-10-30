//go:build linux && arm

package kvm

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/jetkvm/kvm/internal/native"
)

// recordingVideoNative notes the capture calls and serves frames.
type recordingVideoNative struct {
	native.EmptyNativeInterface
	calls []string
}

func (n *recordingVideoNative) VideoStart() error { n.calls = append(n.calls, "start"); return nil }
func (n *recordingVideoNative) VideoStop() error  { n.calls = append(n.calls, "stop"); return nil }
func (n *recordingVideoNative) VideoGetSnapshot() ([]byte, error) {
	n.calls = append(n.calls, "snapshot")
	return []byte("jpeg"), nil
}

func TestScreenshotLeavesAPausedStreamPausedAfterTheLease(t *testing.T) {
	fake := &recordingVideoNative{}
	original := nativeInstance
	nativeInstance = fake
	screenshotLeaseMu.Lock()
	screenshotLeaseUntil = time.Time{}
	screenshotLeaseMu.Unlock()
	videoPauseMu.Lock()
	videoPausedBy = &Session{}
	videoPauseMu.Unlock()
	defer func() {
		nativeInstance = original
		videoPauseMu.Lock()
		videoPausedBy = nil
		videoPauseMu.Unlock()
	}()

	frame, err := captureScreenshot()
	if err != nil || len(frame) == 0 {
		t.Fatalf("frame=%d err=%v", len(frame), err)
	}
	// A cold start skips the first frame, and capture is left running for
	// the lease instead of being stopped at once.
	if want := []string{"start", "snapshot", "snapshot"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	// A second screenshot inside the lease takes a single, fresh frame.
	if _, err := captureScreenshot(); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls[len(fake.calls)-1]; got != "snapshot" || fake.calls[len(fake.calls)-2] != "start" {
		t.Fatalf("calls = %v", fake.calls)
	}

	// Once the lease is over, capture stops because the pause is held.
	screenshotLeaseMu.Lock()
	screenshotLeaseUntil = time.Now().Add(-time.Second)
	if screenshotLeaseTimer != nil {
		screenshotLeaseTimer.Stop()
	}
	screenshotLeaseMu.Unlock()
	releaseScreenshotLease()
	if got := fake.calls[len(fake.calls)-1]; got != "stop" {
		t.Fatalf("capture must stop after the lease while a session holds the pause, calls=%v", fake.calls)
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

func TestScreenshotAuthRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hashed, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	config = &Config{LocalAuthMode: "password", HashedPassword: string(hashed)}
	passwordRateLimiter = NewRateLimiter()
	r := gin.New()
	r.GET("/shot", apiAuthMiddleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
	get := func(password string) int {
		q := httptest.NewRequest(http.MethodGet, "/shot", nil)
		q.SetBasicAuth("api", password)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		return w.Code
	}
	for i := 1; i <= 5; i++ {
		if code := get("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, code)
		}
	}
	if code := get("secret"); code != http.StatusTooManyRequests {
		t.Fatalf("after five failures: %d, want 429", code)
	}
}
