package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterAllows(t *testing.T) {
	l := NewLimiter(10, 10)
	for i := 0; i < 10; i++ {
		if !l.Allow() {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if l.Allow() {
		t.Error("11th request should be rejected")
	}
}

func TestLimiterRefills(t *testing.T) {
	l := NewLimiter(100, 1)
	if !l.Allow() {
		t.Fatal("first request should be allowed")
	}
	if l.Allow() {
		t.Fatal("second request should be rejected")
	}
	time.Sleep(15 * time.Millisecond)
	if !l.Allow() {
		t.Fatal("request after refill should be allowed")
	}
}

func TestLimiterDisabled(t *testing.T) {
	l := NewLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !l.Allow() {
			t.Fatal("should always allow when disabled")
		}
	}
}
