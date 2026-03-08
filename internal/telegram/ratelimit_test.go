package telegram

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsUnderLimit(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.Allow(100) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
}

func TestRateLimiter_BlocksOverLimit(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		rl.Allow(100)
	}
	if rl.Allow(100) {
		t.Error("4th attempt should be blocked")
	}
}

func TestRateLimiter_SeparateUsers(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute)
	rl.Allow(100)
	rl.Allow(100)
	if rl.Allow(100) {
		t.Error("user 100 should be blocked")
	}
	if !rl.Allow(200) {
		t.Error("user 200 should still be allowed")
	}
}

func TestRateLimiter_Cleanup(t *testing.T) {
	rl := NewRateLimiter(2, 10*time.Millisecond)
	rl.Allow(100)
	rl.Allow(100)
	time.Sleep(20 * time.Millisecond)
	rl.Cleanup()
	if !rl.Allow(100) {
		t.Error("should be allowed after window expired and cleanup")
	}
}
