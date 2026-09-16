// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/base64"
	"testing"
	"time"
)

func jwtWithExp(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(exp) + `}`))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestRefreshDeadline(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	// 24h token -> refresh at 80% = 19h12m
	got := refreshDeadline(jwtWithExp(now.Add(24*time.Hour).Unix()), now)
	if want := now.Add(19*time.Hour + 12*time.Minute); !got.Equal(want) {
		t.Errorf("24h: got %s want %s", got, want)
	}
	// 1-minute token -> floor of 5 minutes, never a tight loop
	got = refreshDeadline(jwtWithExp(now.Add(time.Minute).Unix()), now)
	if want := now.Add(5 * time.Minute); !got.Equal(want) {
		t.Errorf("short: got %s want %s", got, want)
	}
	// opaque token -> 12h fallback
	got = refreshDeadline("not-a-jwt", now)
	if want := now.Add(12 * time.Hour); !got.Equal(want) {
		t.Errorf("opaque: got %s want %s", got, want)
	}
}
