package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLocale  = "en_US"
	firefoxVersion = "151.0"
)

// configFilePath returns the per-user config path for name.
func configFilePath(name string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", name)
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, name)
	}
	return name
}

func tokenPath() string    { return configFilePath("deepseek.token") }
func deviceIDPath() string { return configFilePath("deepseek.device") }

// writePrivateFile writes value to path with mode 0600, creating the parent
// directory with mode 0700. It writes through a temporary file and renames it
// into place so a crash cannot leave a truncated token or device id.
func writePrivateFile(path, value string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// resolveDeviceID returns the device id for this install. DEEPSEEK_DEVICE_ID
// wins; otherwise the id is loaded from ~/.config/deepseek.device or generated
// and persisted there.
func resolveDeviceID() string {
	return resolveDeviceIDAt(deviceIDPath())
}

func resolveDeviceIDAt(path string) string {
	if id := normalizeDeviceID(os.Getenv("DEEPSEEK_DEVICE_ID")); id != "" {
		return id
	}

	if data, err := os.ReadFile(path); err == nil {
		if id := normalizeDeviceID(string(data)); id != "" {
			return id
		}
	}

	id, err := newDeviceID()
	if err != nil {
		return ""
	}
	if err := writePrivateFile(path, id+"\n"); err != nil {
		// The generated id still works for this process even if it could not
		// be persisted; the next run will generate another one.
		return id
	}

	// Re-read so two processes starting at once converge on the stored id.
	if data, err := os.ReadFile(path); err == nil {
		if stored := normalizeDeviceID(string(data)); stored != "" {
			return stored
		}
	}
	return id
}

func normalizeDeviceID(value string) string {
	value = strings.TrimSpace(value)
	if !validDeviceID(value) {
		return ""
	}
	return value
}

func validDeviceID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func newDeviceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// systemLocale returns the user's locale in the xx_YY form DeepSeek expects.
func systemLocale() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		value = strings.SplitN(value, ".", 2)[0]
		value = strings.SplitN(value, "@", 2)[0]
		value = strings.ReplaceAll(value, "-", "_")
		if validLocale(value) {
			return value
		}
	}
	return defaultLocale
}

func validLocale(value string) bool {
	parts := strings.Split(value, "_")
	if len(parts) != 2 {
		return false
	}
	if len(parts[0]) < 2 || len(parts[0]) > 3 {
		return false
	}
	if len(parts[1]) < 2 || len(parts[1]) > 4 {
		return false
	}
	for _, part := range parts {
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return false
			}
		}
	}
	return true
}

// timezoneOffset formats a time's UTC offset in seconds, which is what the
// web client sends in x-client-timezone-offset.
func timezoneOffset(at time.Time) string {
	_, offset := at.Zone()
	return strconv.Itoa(offset)
}

func systemTimezoneOffset() string {
	return timezoneOffset(time.Now())
}

// systemUserAgent returns a Firefox user agent for the current platform, or
// DEEPSEEK_USER_AGENT when set.
func systemUserAgent() string {
	if ua := strings.TrimSpace(os.Getenv("DEEPSEEK_USER_AGENT")); ua != "" {
		return ua
	}

	platform := "X11; Linux x86_64"
	switch runtime.GOOS {
	case "darwin":
		platform = "Macintosh; Intel Mac OS X 10.15"
	case "windows":
		platform = "Windows NT 10.0; Win64; x64"
	default:
		if runtime.GOARCH == "arm64" {
			platform = "X11; Linux aarch64"
		}
	}
	return "Mozilla/5.0 (" + platform + "; rv:" + firefoxVersion + ") Gecko/20100101 Firefox/" + firefoxVersion
}
