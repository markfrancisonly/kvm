//go:build linux && arm

package kvm

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/jetkvm/kvm/internal/native"
)

// recordingVideoNative notes the capture calls and serves one frame.
type recordingVideoNative struct {
	native.EmptyNativeInterface
	calls []string
}

func (n *recordingVideoNative) VideoStart() error                 { n.calls = append(n.calls, "start"); return nil }
func (n *recordingVideoNative) VideoStop() error                  { n.calls = append(n.calls, "stop"); return nil }
func (n *recordingVideoNative) VideoGetSnapshot() ([]byte, error) { return []byte("jpeg"), nil }

func TestScreenshotLeavesAPausedStreamPaused(t *testing.T) {
	fake := &recordingVideoNative{}
	original := nativeInstance
	nativeInstance = fake
	defer func() { nativeInstance = original }()
	videoPauseMu.Lock()
	videoPausedBy = &Session{}
	videoPauseMu.Unlock()
	defer func() {
		videoPauseMu.Lock()
		videoPausedBy = nil
		videoPauseMu.Unlock()
	}()

	frame, err := captureScreenshot()
	if err != nil || len(frame) == 0 {
		t.Fatalf("frame=%d err=%v", len(frame), err)
	}
	if len(fake.calls) == 0 || fake.calls[len(fake.calls)-1] != "stop" {
		t.Fatalf("capture must end stopped while a session holds the pause, calls=%v", fake.calls)
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
