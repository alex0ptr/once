package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// cacheKey derives the cache key as HMAC-SHA256(tenant, dir ‖ argv). Every
// field is length-prefixed, so ["a b"] and ["a", "b"] never collide. The
// tenant key acts as a namespace: it separates cache contexts and makes keys
// practically unguessable, but it is not a secret and authenticates nothing.
// An empty dir (--no-dir) cannot collide with a real one, since os.Getwd
// never returns "".
func cacheKey(tenant, dir string, argv []string) string {
	m := hmac.New(sha256.New, []byte(tenant))
	field := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		m.Write(n[:])
		m.Write([]byte(s))
	}
	field("once/v1")
	field(dir)
	for _, a := range argv {
		field(a)
	}
	return hex.EncodeToString(m.Sum(nil))
}

func validKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	_, err := hex.DecodeString(k)
	return err == nil
}

// parseUntil accepts RFC 3339 timestamps (with or without seconds, with
// "+02:00" or "+0200" zones) and local date/time forms without a zone.
func parseUntil(s string) (time.Time, error) {
	zoned := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04Z07:00",
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04-0700",
	}
	for _, l := range zoned {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	local := []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"}
	for _, l := range local {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --until %q (want e.g. 2026-10-09T06:00 or 2026-10-09T06:00:00+02:00)", s)
}

// expiry returns the earlier of now+ttl and until (if set).
func expiry(now time.Time, ttl time.Duration, until time.Time) time.Time {
	exp := now.Add(ttl).Round(0) // wall clock only
	if !until.IsZero() && until.Before(exp) {
		exp = until
	}
	return exp
}
