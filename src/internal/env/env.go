// Package env reads service configuration from the environment.
//
// Readers return an error that names the variable instead of panicking or
// silently substituting zero: the service must refuse to start on a broken
// environment, and the log must show which variable is wrong.
//
// Numbers are positive, and "no value" is expressed by an empty variable, not
// by zero: in all services integer settings are limits, counters and
// identifiers where zero has no meaning, and switches are a separate Bool.
package env

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// String returns the variable value, or fallback if it is empty.
func String(name, fallback string) string {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		return raw
	}
	return fallback
}

// RequireString returns an error if the variable is empty.
func RequireString(name string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return raw, nil
}

// Int reads a positive integer; an empty variable gives fallback.
func Int(name string, fallback int) (int, error) {
	value, err := int64Value(name, int64(fallback))
	return int(value), err
}

// RequireInt reads a positive integer and requires it to be present.
func RequireInt(name string) (int, error) {
	value, err := RequireInt64(name)
	return int(value), err
}

// Int64 reads a positive integer; an empty variable gives fallback.
func Int64(name string, fallback int64) (int64, error) {
	return int64Value(name, fallback)
}

// RequireInt64 reads a positive integer and requires it to be present.
func RequireInt64(name string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	return int64Value(name, 0)
}

func int64Value(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

// Int64List reads a comma-separated list; an empty variable gives nil.
func Int64List(name string) ([]int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	values := make([]int64, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("%s must be a comma-separated list of positive integers", name)
		}
		values = append(values, value)
	}
	return values, nil
}

// RequireInt64List reads a comma-separated list and requires it to be present.
func RequireInt64List(name string) ([]int64, error) {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	return Int64List(name)
}

// Float reads a non-negative float; an explicit zero is allowed as a
// deliberate switch-off of a threshold.
func Float(name string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number", name)
	}
	return value, nil
}

// Duration reads a positive duration in time.ParseDuration format.
func Duration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

// Bool reads a switch in strconv.ParseBool format.
func Bool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

// Level reads the log level.
func Level(name string, fallback slog.Level) (slog.Level, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s must be debug, info, warn or error", name)
	}
	return level, nil
}

// ProxyURL reads an HTTP proxy address; an empty variable gives nil without an
// error. Parsing happens at startup, not at the first request: a bad address
// must fail the configuration, not a single call an hour into the run.
func ProxyURL(name string) (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be a proxy URL like http://host:port", name)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
		return parsed, nil
	default:
		return nil, fmt.Errorf("%s has unsupported scheme %q", name, parsed.Scheme)
	}
}
