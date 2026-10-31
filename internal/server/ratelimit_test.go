package server

import (
	"testing"
	"time"
)

func TestAuthLimiterWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newAuthLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if l.blocked("a") != 0 {
			t.Fatalf("blocked after %d failures", i)
		}
		if got := l.fail("a"); got != (i == 2) {
			t.Fatalf("fail #%d reported block=%v", i+1, got)
		}
	}
	if wait := l.blocked("a"); wait != time.Minute {
		t.Fatalf("wait = %v", wait)
	}
	if l.blocked("b") != 0 {
		t.Fatal("other IP blocked")
	}

	now = now.Add(time.Minute)
	if l.blocked("a") != 0 {
		t.Fatal("still blocked after the window")
	}
	l.fail("a") // starts a fresh window
	if l.blocked("a") != 0 {
		t.Fatal("old failures carried over")
	}
	l.fail("a")
	l.succeed("a")
	l.fail("a")
	l.fail("a")
	if l.blocked("a") != 0 {
		t.Fatal("success did not reset the counter")
	}
}
