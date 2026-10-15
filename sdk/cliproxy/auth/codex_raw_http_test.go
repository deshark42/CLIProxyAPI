package auth

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRawHTTPRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, value string
		want        time.Duration
		valid       bool
	}{
		{"seconds", "3600", time.Hour, true},
		{"whitespace", " 60 ", time.Minute, true},
		{"zero", "0", 0, true},
		{"http date", now.Add(time.Hour).Format(http.TimeFormat), time.Hour, true},
		{"past date", now.Add(-time.Hour).Format(http.TimeFormat), 0, false},
		{"negative", "-1", 0, false},
		{"empty", "", 0, false},
		{"invalid", "later", 0, false},
		{"overflow duration", "9223372037", 0, false},
		{"overflow integer", "99999999999999999999", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rawHTTPRetryAfter(tc.value, now)
			if tc.valid {
				if got == nil || *got != tc.want {
					t.Fatalf("retry delay = %v, want %v", got, tc.want)
				}
			} else if got != nil {
				t.Fatalf("retry delay = %v, want nil", *got)
			}
		})
	}
}

func TestRawHTTPResultBodyCompletesOnce(t *testing.T) {
	for _, earlyClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "early close"}[earlyClose], func(t *testing.T) {
			calls := 0
			var outcome error
			body := &rawCodexResultBody{ReadCloser: io.NopCloser(strings.NewReader("body")), complete: func(err error) { calls++; outcome = err }}
			if !earlyClose {
				got, err := io.ReadAll(body)
				if err != nil || string(got) != "body" {
					t.Fatalf("body=%q err=%v", got, err)
				}
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("completion calls = %d", calls)
			}
			if earlyClose && !errors.Is(outcome, io.ErrUnexpectedEOF) {
				t.Fatalf("outcome = %v", outcome)
			}
			if !earlyClose && outcome != nil {
				t.Fatalf("outcome = %v", outcome)
			}
		})
	}
}
