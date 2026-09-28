// Package env читает конфигурацию сервиса из окружения.
//
// Читатели возвращают ошибку с именем переменной, а не паникуют и не
// подставляют молчаливый ноль: сервис обязан не стартовать на кривом
// окружении, и в логе должно быть видно, какая именно переменная кривая.
//
// Числа считаются положительными, а отсутствие значения выражается пустой
// переменной, а не нулём: во всех сервисах целые настройки - это
// лимиты, счётчики и идентификаторы, где ноль не имеет смысла, а выключатели
// сделаны отдельным Bool.
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

// String возвращает значение переменной либо fallback, если она пуста.
func String(name, fallback string) string {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		return raw
	}
	return fallback
}

// RequireString возвращает ошибку, если переменная пуста.
func RequireString(name string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return raw, nil
}

// Int читает положительное целое; пустая переменная даёт fallback.
func Int(name string, fallback int) (int, error) {
	value, err := int64Value(name, int64(fallback))
	return int(value), err
}

// RequireInt читает положительное целое и требует его наличия.
func RequireInt(name string) (int, error) {
	value, err := RequireInt64(name)
	return int(value), err
}

// Int64 читает положительное целое; пустая переменная даёт fallback.
func Int64(name string, fallback int64) (int64, error) {
	return int64Value(name, fallback)
}

// RequireInt64 читает положительное целое и требует его наличия.
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

// Int64List читает список через запятую; пустая переменная даёт nil.
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

// RequireInt64List читает список через запятую и требует его наличия.
func RequireInt64List(name string) ([]int64, error) {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	return Int64List(name)
}

// Float читает неотрицательное дробное; явный ноль допустим как осознанный
// выключатель порога.
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

// Duration читает положительную длительность в формате time.ParseDuration.
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

// Bool читает выключатель в формате strconv.ParseBool.
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

// Level читает уровень логирования.
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

// ProxyURL читает адрес HTTP-прокси; пустая переменная даёт nil без ошибки.
// Разбор идёт на старте, а не в момент первого запроса: кривой адрес обязан
// уронить конфигурацию, а не отдельный вызов через час работы.
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
