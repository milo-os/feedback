package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	if ok, _ := (*Limiter)(nil).Allow("x"); !ok {
		t.Fatal("nil limiter must allow")
	}
	now := time.Unix(0, 0)
	l := NewLimiter(60, 2) // one a second, bursts of two
	l.now = func() time.Time { return now }
	for i, want := range []bool{true, true, false} {
		if ok, _ := l.Allow("a"); ok != want {
			t.Fatalf("call %d: %v", i, ok)
		}
	}
	if ok, wait := l.Allow("a"); ok || wait <= 0 || wait > time.Second {
		t.Fatalf("wait %v", wait)
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("token not refilled")
	}
	now = now.Add(idle + time.Second)
	l.Allow("b")
	if _, ok := l.users["a"]; ok {
		t.Error("idle bucket not swept")
	}
}
