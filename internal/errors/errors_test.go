package errors

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func TestAPIErrorError(t *testing.T) {
	err := &APIError{HTTPStatus: http.StatusTeapot, Code: "TEAPOT", Message: "short and stout"}
	if err.Error() != "short and stout" {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestNewAPIError(t *testing.T) {
	got := NewAPIError(ErrValidation, "name is required")

	if got.HTTPStatus != ErrValidation.HTTPStatus || got.Code != ErrValidation.Code {
		t.Errorf("status/code not inherited from base: %+v", got)
	}
	if got.Message != "name is required" {
		t.Errorf("Message = %q", got.Message)
	}
	if ErrValidation.Message == "name is required" {
		t.Error("NewAPIError mutated the base error")
	}
}

func TestNewAPIErrorWithUpstream(t *testing.T) {
	got := NewAPIErrorWithUpstream(http.StatusTooManyRequests, "UPSTREAM_RATE_LIMIT", "slow down")

	want := APIError{HTTPStatus: http.StatusTooManyRequests, Code: "UPSTREAM_RATE_LIMIT", Message: "slow down"}
	if *got != want {
		t.Errorf("NewAPIErrorWithUpstream() = %+v, want %+v", *got, want)
	}
}

func TestParseDBError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want *APIError
	}{
		{"nil error", nil, nil},
		{"record not found", gorm.ErrRecordNotFound, ErrResourceNotFound},
		{"wrapped record not found", fmt.Errorf("find group: %w", gorm.ErrRecordNotFound), ErrResourceNotFound},
		{"postgres unique violation", &pgconn.PgError{Code: "23505"}, ErrDuplicateResource},
		{"postgres other error", &pgconn.PgError{Code: "42P01"}, ErrDatabase},
		{"mysql duplicate entry", &mysql.MySQLError{Number: 1062}, ErrDuplicateResource},
		{"mysql other error", &mysql.MySQLError{Number: 1146}, ErrDatabase},
		{"sqlite unique constraint", errors.New("UNIQUE constraint failed: groups.name"), ErrDuplicateResource},
		{"unknown error", errors.New("connection refused"), ErrDatabase},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseDBError(tt.err); got != tt.want {
				t.Errorf("ParseDBError() = %v, want %v", got, tt.want)
			}
		})
	}
}
