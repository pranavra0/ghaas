package main

import (
	"testing"
	"time"

	"ghaas/pkg/manifest"
)

func TestScheduleKeyPreservesCommonGranularities(t *testing.T) {
	now := time.Date(2026, 8, 29, 14, 37, 0, 0, time.UTC)
	cases := []struct {
		name, cron, want string
	}{
		{"daily", "15 9 * * *", "2026-08-29"},
		{"hourly", "0 * * * *", "2026-08-29T14"},
		{"weekly", "15 9 * * 6", "2026-W35"},
		{"multi-hour", "0 9,17 * * *", "2026-08-29T14:37+00:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduleKey(now, tc.cron); got != tc.want {
				t.Fatalf("scheduleKey(%q) = %q, want %q", tc.cron, got, tc.want)
			}
		})
	}
}

func TestTargetScheduleKeysAndWeekdayWindow(t *testing.T) {
	friday := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if got := targetScheduleKey(friday, "09:15 Friday"); got != "2026-W35" {
		t.Fatalf("targetScheduleKey() = %q, want 2026-W35", got)
	}
	schedule := &manifest.ScheduleConfig{
		Target:   "09:15 Friday",
		Timezone: "UTC",
		Retry:    &manifest.ScheduleRetryConfig{Every: manifest.Duration(5 * time.Minute), Until: "19:00"},
	}
	if !scheduleExecutionAllowed(schedule, friday) {
		t.Fatal("expected Friday target inside window to be allowed")
	}
	saturday := friday.Add(24 * time.Hour)
	if scheduleExecutionAllowed(schedule, saturday) {
		t.Fatal("expected non-target weekday to be rejected")
	}
}

func TestScheduleExecutionWindow(t *testing.T) {
	schedule := &manifest.ScheduleConfig{
		Timezone:        "UTC",
		ExecutionWindow: &manifest.ExecutionWindow{Start: "09:15", End: "19:00"},
	}
	if !scheduleExecutionAllowed(schedule, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected time inside execution window to be allowed")
	}
	if scheduleExecutionAllowed(schedule, time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)) {
		t.Fatal("expected time outside execution window to be rejected")
	}
}
