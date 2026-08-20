package utils

import (
	"gpt-load/internal/models"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResolveHeaderVariablesNilContext(t *testing.T) {
	if got := ResolveHeaderVariables("${CLIENT_IP}", nil); got != "${CLIENT_IP}" {
		t.Errorf("nil context should leave value untouched, got %q", got)
	}
}

func TestResolveHeaderVariables(t *testing.T) {
	ctx := &HeaderVariableContext{
		ClientIP: "10.0.0.1",
		Group:    &models.Group{Name: "my-group"},
		APIKey:   &models.APIKey{KeyValue: "sk-abc"},
	}

	if got := ResolveHeaderVariables("ip=${CLIENT_IP};group=${GROUP_NAME};key=${API_KEY}", ctx); got != "ip=10.0.0.1;group=my-group;key=sk-abc" {
		t.Errorf("ResolveHeaderVariables() = %q", got)
	}

	if got := ResolveHeaderVariables("${CLIENT_IP},${CLIENT_IP}", ctx); got != "10.0.0.1,10.0.0.1" {
		t.Errorf("all occurrences should be replaced, got %q", got)
	}

	if got := ResolveHeaderVariables("static", ctx); got != "static" {
		t.Errorf("value without variables changed: %q", got)
	}

	if got := ResolveHeaderVariables("${UNKNOWN}", ctx); got != "${UNKNOWN}" {
		t.Errorf("unknown variable should be left as-is, got %q", got)
	}
}

func TestResolveHeaderVariablesTimestamps(t *testing.T) {
	before := time.Now()
	got := ResolveHeaderVariables("${TIMESTAMP_S}|${TIMESTAMP_MS}", &HeaderVariableContext{})
	after := time.Now()

	parts := strings.Split(got, "|")
	if len(parts) != 2 {
		t.Fatalf("unexpected result %q", got)
	}

	secs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("TIMESTAMP_S not numeric: %q", parts[0])
	}
	if secs < before.Unix() || secs > after.Unix() {
		t.Errorf("TIMESTAMP_S %d outside [%d, %d]", secs, before.Unix(), after.Unix())
	}

	millis, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("TIMESTAMP_MS not numeric: %q", parts[1])
	}
	if millis < before.UnixMilli() || millis > after.UnixMilli() {
		t.Errorf("TIMESTAMP_MS %d outside [%d, %d]", millis, before.UnixMilli(), after.UnixMilli())
	}
}

func TestResolveHeaderVariablesNilGroupAndKey(t *testing.T) {
	got := ResolveHeaderVariables("${GROUP_NAME}/${API_KEY}", &HeaderVariableContext{ClientIP: "1.2.3.4"})
	if got != "${GROUP_NAME}/${API_KEY}" {
		t.Errorf("group/key variables should be untouched when nil, got %q", got)
	}
}

func TestApplyHeaderRules(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Remove-Me", "value")
	req.Header.Set("X-Overwrite", "old")

	rules := []models.HeaderRule{
		{Key: "x-remove-me", Action: "remove"},
		{Key: "x-overwrite", Value: "new", Action: "set"},
		{Key: "x-client-ip", Value: "${CLIENT_IP}", Action: "set"},
		{Key: "x-ignored", Value: "v", Action: "unknown"},
	}

	ApplyHeaderRules(req, rules, &HeaderVariableContext{ClientIP: "10.0.0.1"})

	if got := req.Header.Get("X-Remove-Me"); got != "" {
		t.Errorf("header not removed: %q", got)
	}
	if got := req.Header.Get("X-Overwrite"); got != "new" {
		t.Errorf("X-Overwrite = %q, want %q", got, "new")
	}
	if got := req.Header.Get("X-Client-Ip"); got != "10.0.0.1" {
		t.Errorf("X-Client-Ip = %q, want %q", got, "10.0.0.1")
	}
	if _, exists := req.Header["X-Ignored"]; exists {
		t.Error("unknown action should be a no-op")
	}
}

func TestApplyHeaderRulesNoops(t *testing.T) {
	// nil 请求或空规则不应 panic
	ApplyHeaderRules(nil, []models.HeaderRule{{Key: "a", Action: "set"}}, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ApplyHeaderRules(req, nil, nil)
	if len(req.Header) != 0 {
		t.Errorf("headers modified with empty rules: %v", req.Header)
	}
}

func TestNewHeaderVariableContext(t *testing.T) {
	group := &models.Group{Name: "g"}
	key := &models.APIKey{KeyValue: "sk-1"}

	ctx := NewHeaderVariableContext(group, key)
	if ctx.ClientIP != "127.0.0.1" || ctx.Group != group || ctx.APIKey != key {
		t.Errorf("NewHeaderVariableContext() = %+v", ctx)
	}
}
