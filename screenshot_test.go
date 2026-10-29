//go:build linux && arm

package kvm

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

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
