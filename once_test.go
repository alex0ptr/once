package main

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestCacheKeySeparation(t *testing.T) {
	base := cacheKey("t", "/d", []string{"a", "b"})
	cases := map[string]string{
		"joined args":  cacheKey("t", "/d", []string{"a b"}),
		"other tenant": cacheKey("u", "/d", []string{"a", "b"}),
		"other dir":    cacheKey("t", "/e", []string{"a", "b"}),
		"shifted":      cacheKey("t", "/da", []string{"", "b"}),
		"no dir":       cacheKey("t", "", []string{"a", "b"}),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("%s: key collides", name)
		}
	}
	if !validKey(base) {
		t.Error("generated key not valid")
	}
}

func TestParseUntil(t *testing.T) {
	for _, s := range []string{
		"2026-10-09T06:00:00+02:00", "2026-10-09T06:00+02:00", "2026-10-09T06:00:00+0200",
		"2026-10-09T06:00:00Z", "2026-10-09T06:00:00", "2026-10-09T06:00", "2026-10-09",
	} {
		if _, err := parseUntil(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := parseUntil("tomorrow"); err == nil {
		t.Error("expected error for relative value")
	}
}

func TestExpiryTakesEarlier(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	if got := expiry(now, 24*time.Hour, until); !got.Equal(until) {
		t.Errorf("want until, got %v", got)
	}
	if got := expiry(now, 30*time.Minute, until); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("want ttl, got %v", got)
	}
}

func TestStoreExpiryAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idleGrace)
		k := cacheKey("t", "/", []string{"x"})
		s.put(k, []byte("secret"), time.Now().Add(time.Hour).Round(0))
		if v, ok := s.get(k, time.Now()); !ok || string(v) != "secret" {
			t.Fatal("expected hit")
		}

		time.Sleep(time.Hour - time.Millisecond)
		synctest.Wait()
		select {
		case <-s.done:
			t.Fatal("stopped too early")
		default:
		}

		time.Sleep(time.Millisecond)
		synctest.Wait()
		select {
		case <-s.done:
		default:
			t.Fatal("daemon did not stop after last entry expired")
		}
		if _, ok := s.get(k, time.Now()); ok {
			t.Fatal("entry survived shutdown")
		}
	})
}

func TestStoreIdleGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idleGrace)
		time.Sleep(idleGrace)
		synctest.Wait()
		select {
		case <-s.done:
		default:
			t.Fatal("empty daemon did not stop after the grace period")
		}
	})
}

func TestStoreWipesAtExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(idleGrace)
		long := cacheKey("t", "/", []string{"long"})
		short := cacheKey("t", "/", []string{"short"})
		s.put(long, []byte("long"), time.Now().Add(time.Hour).Round(0))
		data := []byte("short")
		s.put(short, data, time.Now().Add(10*time.Second).Round(0)) // earlier than the timer target

		time.Sleep(10 * time.Second)
		synctest.Wait()
		s.mu.Lock()
		_, shortLeft := s.entries[short]
		_, longLeft := s.entries[long]
		s.mu.Unlock()
		if shortLeft || string(data) != "\x00\x00\x00\x00\x00" {
			t.Fatalf("short entry not wiped at expiry (present=%v, data=%q)", shortLeft, data)
		}
		if !longLeft {
			t.Fatal("long entry dropped too early")
		}
		select {
		case <-s.done:
			t.Fatal("daemon stopped while an entry is left")
		default:
		}
	})
}
