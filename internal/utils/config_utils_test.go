package utils

import (
	"gpt-load/internal/models"
	"gpt-load/internal/types"
	"reflect"
	"testing"
)

func TestParseInteger(t *testing.T) {
	tests := []struct {
		value        string
		defaultValue int
		want         int
	}{
		{"", 7, 7},
		{"42", 7, 42},
		{"-3", 7, -3},
		{"0", 7, 0},
		{"abc", 7, 7},
		{"1.5", 7, 7},
	}

	for _, tt := range tests {
		if got := ParseInteger(tt.value, tt.defaultValue); got != tt.want {
			t.Errorf("ParseInteger(%q, %d) = %d, want %d", tt.value, tt.defaultValue, got, tt.want)
		}
	}
}

func TestParseBoolean(t *testing.T) {
	tests := []struct {
		value        string
		defaultValue bool
		want         bool
	}{
		{"", true, true},
		{"", false, false},
		{"true", false, true},
		{"TRUE", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"On", false, true},
		{"false", true, false},
		{"0", true, false},
		{"no", true, false},
		{"OFF", true, false},
		{"maybe", true, true},
		{"maybe", false, false},
	}

	for _, tt := range tests {
		if got := ParseBoolean(tt.value, tt.defaultValue); got != tt.want {
			t.Errorf("ParseBoolean(%q, %v) = %v, want %v", tt.value, tt.defaultValue, got, tt.want)
		}
	}
}

func TestParseArray(t *testing.T) {
	def := []string{"fallback"}

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{"empty falls back", "", def},
		{"only separators falls back", " , , ", def},
		{"single value", "a", []string{"a"}},
		{"trims and drops empties", " a , b ,, c ", []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseArray(tt.value, def); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseArray(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestGetEnvOrDefault(t *testing.T) {
	t.Setenv("GPT_LOAD_TEST_ENV", "value")
	if got := GetEnvOrDefault("GPT_LOAD_TEST_ENV", "fallback"); got != "value" {
		t.Errorf("GetEnvOrDefault() = %q, want %q", got, "value")
	}

	t.Setenv("GPT_LOAD_TEST_ENV", "")
	if got := GetEnvOrDefault("GPT_LOAD_TEST_ENV", "fallback"); got != "fallback" {
		t.Errorf("empty env var should fall back, got %q", got)
	}

	if got := GetEnvOrDefault("GPT_LOAD_TEST_ENV_UNSET", "fallback"); got != "fallback" {
		t.Errorf("unset env var should fall back, got %q", got)
	}
}

func TestSetFieldFromString(t *testing.T) {
	target := struct {
		IntField    int
		BoolField   bool
		StringField string
		FloatField  float64
	}{}
	v := reflect.ValueOf(&target).Elem()

	if err := SetFieldFromString(v.FieldByName("IntField"), "12"); err != nil || target.IntField != 12 {
		t.Errorf("int: got %d, err %v", target.IntField, err)
	}
	if err := SetFieldFromString(v.FieldByName("BoolField"), "true"); err != nil || !target.BoolField {
		t.Errorf("bool: got %v, err %v", target.BoolField, err)
	}
	if err := SetFieldFromString(v.FieldByName("StringField"), "hello"); err != nil || target.StringField != "hello" {
		t.Errorf("string: got %q, err %v", target.StringField, err)
	}

	if err := SetFieldFromString(v.FieldByName("IntField"), "not-an-int"); err == nil {
		t.Error("expected error for invalid integer")
	}
	if err := SetFieldFromString(v.FieldByName("BoolField"), "not-a-bool"); err == nil {
		t.Error("expected error for invalid boolean")
	}
	if err := SetFieldFromString(v.FieldByName("FloatField"), "1.5"); err == nil {
		t.Error("expected error for unsupported field kind")
	}

	// 未导出/不可寻址的字段无法设置
	if err := SetFieldFromString(reflect.ValueOf(target).FieldByName("IntField"), "1"); err == nil {
		t.Error("expected error for non-settable field")
	}
}

func TestDefaultSystemSettingsAppliesDefaultTags(t *testing.T) {
	s := DefaultSystemSettings()
	v := reflect.ValueOf(&s).Elem()
	tp := v.Type()

	checked := 0
	for i := range tp.NumField() {
		field := tp.Field(i)
		defaultTag := field.Tag.Get("default")
		if defaultTag == "" {
			continue
		}

		expected := reflect.New(field.Type).Elem()
		if err := SetFieldFromString(expected, defaultTag); err != nil {
			continue // 不支持的类型，DefaultSystemSettings 仅告警
		}
		if !reflect.DeepEqual(v.Field(i).Interface(), expected.Interface()) {
			t.Errorf("field %s = %v, want default tag value %v", field.Name, v.Field(i).Interface(), expected.Interface())
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("no fields with default tags found; test is vacuous")
	}
}

func TestGenerateSettingsMetadata(t *testing.T) {
	s := DefaultSystemSettings()
	infos := GenerateSettingsMetadata(&s)

	if len(infos) == 0 {
		t.Fatal("GenerateSettingsMetadata returned no entries")
	}

	byKey := make(map[string]models.SystemSettingInfo, len(infos))
	for _, info := range infos {
		if info.Key == "" || info.Key == "-" {
			t.Errorf("entry with invalid key: %+v", info)
		}
		if _, dup := byKey[info.Key]; dup {
			t.Errorf("duplicate key %q", info.Key)
		}
		byKey[info.Key] = info
	}

	// 交叉校验带 validate 标签的字段，确认 min/required 解析正确
	tp := reflect.TypeOf(types.SystemSettings{})
	for i := range tp.NumField() {
		field := tp.Field(i)
		jsonTag := field.Tag.Get("json")
		info, ok := byKey[jsonTag]
		if jsonTag == "" || jsonTag == "-" {
			if ok {
				t.Errorf("field %s without json tag should be skipped", field.Name)
			}
			continue
		}
		if !ok {
			t.Fatalf("field %s (json %q) missing from metadata", field.Name, jsonTag)
		}
		if info.Type != field.Type.String() {
			t.Errorf("%s Type = %q, want %q", jsonTag, info.Type, field.Type.String())
		}
		if info.DefaultValue != field.Tag.Get("default") {
			t.Errorf("%s DefaultValue = %q, want %q", jsonTag, info.DefaultValue, field.Tag.Get("default"))
		}
		if info.Name != field.Tag.Get("name") || info.Description != field.Tag.Get("desc") || info.Category != field.Tag.Get("category") {
			t.Errorf("%s metadata tags mismatch: %+v", jsonTag, info)
		}
	}
}

func TestGetValidationEndpoint(t *testing.T) {
	tests := []struct {
		name  string
		group *models.Group
		want  string
	}{
		{"explicit endpoint wins", &models.Group{ValidationEndpoint: "/custom", ChannelType: "openai"}, "/custom"},
		{"openai default", &models.Group{ChannelType: "openai"}, "/v1/chat/completions"},
		{"openai-response default", &models.Group{ChannelType: "openai-response"}, "/v1/responses"},
		{"anthropic default", &models.Group{ChannelType: "anthropic"}, "/v1/messages"},
		{"unknown channel type", &models.Group{ChannelType: "gemini"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetValidationEndpoint(tt.group); got != tt.want {
				t.Errorf("GetValidationEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}
