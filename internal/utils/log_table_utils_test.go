package utils

import (
	"reflect"
	"testing"
	"time"
)

func TestValidateLogTableName(t *testing.T) {
	valid := []string{"request_logs_20260101", "request_logs_99999999"}
	for _, name := range valid {
		if !ValidateLogTableName(name) {
			t.Errorf("ValidateLogTableName(%q) = false, want true", name)
		}
	}

	invalid := []string{
		"",
		"request_logs",
		"request_logs_",
		"request_logs_2026010",   // 7 digits
		"request_logs_202601011", // 9 digits
		"request_logs_2026010a",  // non-digit
		"logs_20260101",          // wrong prefix
		"xrequest_logs_20260101", // prefix not anchored
		"request_logs_20260101x", // suffix not anchored
		"request_logs_20260101; DROP TABLE users",
	}
	for _, name := range invalid {
		if ValidateLogTableName(name) {
			t.Errorf("ValidateLogTableName(%q) = true, want false", name)
		}
	}
}

func TestGetDailyLogTableName(t *testing.T) {
	date := time.Date(2026, 4, 18, 23, 59, 59, 0, time.UTC)
	if got := GetDailyLogTableName(date); got != "request_logs_20260418" {
		t.Errorf("GetDailyLogTableName() = %q", got)
	}
}

func TestIsTodayLogTable(t *testing.T) {
	today := GetDailyLogTableName(time.Now())
	if !IsTodayLogTable(today) {
		t.Errorf("IsTodayLogTable(%q) = false, want true", today)
	}
	yesterday := GetDailyLogTableName(time.Now().AddDate(0, 0, -1))
	if IsTodayLogTable(yesterday) {
		t.Errorf("IsTodayLogTable(%q) = true, want false", yesterday)
	}
}

func TestGetLogTablesForDateRange(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)

	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  []string
	}{
		{
			name:  "same day",
			start: time.Date(2026, 4, 18, 1, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 4, 18, 23, 0, 0, 0, time.UTC),
			want:  []string{"request_logs_20260418"},
		},
		{
			name:  "spans three days",
			start: time.Date(2026, 4, 18, 22, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 4, 20, 3, 0, 0, 0, time.UTC),
			want:  []string{"request_logs_20260418", "request_logs_20260419", "request_logs_20260420"},
		},
		{
			name:  "crosses month boundary",
			start: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			want:  []string{"request_logs_20260430", "request_logs_20260501"},
		},
		{
			name:  "uses local date not UTC truncation",
			start: time.Date(2026, 4, 18, 2, 0, 0, 0, loc),
			end:   time.Date(2026, 4, 18, 5, 0, 0, 0, loc),
			want:  []string{"request_logs_20260418"},
		},
		{
			name:  "end before start yields nothing",
			start: time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC),
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetLogTablesForDateRange(tt.start, tt.end); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLogTablesForDateRange() = %v, want %v", got, tt.want)
			}
		})
	}
}
