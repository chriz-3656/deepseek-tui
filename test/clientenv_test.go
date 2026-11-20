package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWritePrivateFileCreatesAndTightensPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "secret")

	if err := writePrivateFile(path, "first\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first\n" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode = %o, want 600", got)
		}
	}

	// A pre-existing, world-readable file must end up private after rewrite.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	if err := writePrivateFile(path, "second\n"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat after rewrite: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("rewritten mode = %o, want 600", got)
		}
	}
}

func TestResolveDeviceIDEnvOverrideWins(t *testing.T) {
	t.Setenv("DEEPSEEK_DEVICE_ID", "env-device-123")
	path := filepath.Join(t.TempDir(), "deepseek.device")

	if got := resolveDeviceIDAt(path); got != "env-device-123" {
		t.Fatalf("device id = %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("env override should not create a file, stat err = %v", err)
	}
}

func TestResolveDeviceIDPersistsAndReuses(t *testing.T) {
	t.Setenv("DEEPSEEK_DEVICE_ID", "")
	path := filepath.Join(t.TempDir(), "deepseek.device")

	first := resolveDeviceIDAt(path)
	if !validDeviceID(first) {
		t.Fatalf("generated id is invalid: %q", first)
	}
	if first[14] != '4' {
		t.Fatalf("generated id is not UUIDv4: %q", first)
	}
	if !strings.ContainsRune("89ab", rune(first[19])) {
		t.Fatalf("generated id has wrong variant nibble: %q", first)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("device file: %v", err)
	}
	if strings.TrimSpace(string(stored)) != first {
		t.Fatalf("stored id = %q, want %q", stored, first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode = %o, want 600", got)
		}
	}

	if second := resolveDeviceIDAt(path); second != first {
		t.Fatalf("second resolve = %q, want %q", second, first)
	}
}

func TestResolveDeviceIDReplacesInvalidFile(t *testing.T) {
	t.Setenv("DEEPSEEK_DEVICE_ID", "")
	path := filepath.Join(t.TempDir(), "deepseek.device")
	if err := os.WriteFile(path, []byte("not a device id!"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got := resolveDeviceIDAt(path)
	if !validDeviceID(got) || strings.Contains(got, "not a device id") {
		t.Fatalf("invalid stored id was not replaced: %q", got)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(string(stored)) != got {
		t.Fatalf("stored = %q, want %q", stored, got)
	}
}

func TestSystemLocale(t *testing.T) {
	cases := []struct {
		name     string
		lcAll    string
		messages string
		lang     string
		want     string
	}{
		{"utf8 suffix", "de_DE.UTF-8", "", "en_US.UTF-8", "de_DE"},
		{"dash form", "", "fr-CA.UTF-8", "en_US", "fr_CA"},
		{"modifier", "", "", "sr_RS@latin", "sr_RS"},
		{"lc_all precedence", "ja_JP.UTF-8", "de_DE.UTF-8", "en_GB.UTF-8", "ja_JP"},
		{"messages fallback", "", "pt_BR.UTF-8", "en_GB.UTF-8", "pt_BR"},
		{"invalid falls through", "C", "", "es_ES.UTF-8", "es_ES"},
		{"default", "", "", "", "en_US"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LC_ALL", tc.lcAll)
			t.Setenv("LC_MESSAGES", tc.messages)
			t.Setenv("LANG", tc.lang)
			if got := systemLocale(); got != tc.want {
				t.Fatalf("locale = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTimezoneOffset(t *testing.T) {
	cases := []struct {
		name string
		zone *time.Location
		want string
	}{
		{"east", time.FixedZone("EAT", 3*3600), "10800"},
		{"west", time.FixedZone("EST", -5*3600), "-18000"},
		{"half hour", time.FixedZone("IST", 5*3600+1800), "19800"},
		{"utc", time.UTC, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timezoneOffset(time.Now().In(tc.zone)); got != tc.want {
				t.Fatalf("offset = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSystemUserAgent(t *testing.T) {
	t.Setenv("DEEPSEEK_USER_AGENT", "custom-agent/1.0")
	if got := systemUserAgent(); got != "custom-agent/1.0" {
		t.Fatalf("override = %q", got)
	}

	t.Setenv("DEEPSEEK_USER_AGENT", "")
	got := systemUserAgent()
	if !strings.HasPrefix(got, "Mozilla/5.0 (") || !strings.Contains(got, "Firefox/"+firefoxVersion) {
		t.Fatalf("platform agent = %q", got)
	}
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(got, "Macintosh") {
			t.Fatalf("darwin agent = %q", got)
		}
	case "windows":
		if !strings.Contains(got, "Windows NT") {
			t.Fatalf("windows agent = %q", got)
		}
	default:
		if !strings.Contains(got, "Linux") {
			t.Fatalf("linux agent = %q", got)
		}
	}
}

func TestValidDeviceIDAndLocale(t *testing.T) {
	validIDs := []string{"abc12345", "11111111-2222-4333-8444-555555555555", strings.Repeat("a", 128)}
	for _, id := range validIDs {
		if !validDeviceID(id) {
			t.Fatalf("valid id rejected: %q", id)
		}
	}
	for _, id := range []string{"", "short", strings.Repeat("a", 129), "bad id", "semi;colon"} {
		if validDeviceID(id) {
			t.Fatalf("invalid id accepted: %q", id)
		}
	}

	for _, locale := range []string{"en_US", "de_DE", "sr_RS", "zh_Hant"} {
		if !validLocale(locale) {
			t.Fatalf("valid locale rejected: %q", locale)
		}
	}
	for _, locale := range []string{"", "en", "en_US_POSIX", "e_N", "en_U5", "en-US"} {
		if validLocale(locale) {
			t.Fatalf("invalid locale accepted: %q", locale)
		}
	}
}
