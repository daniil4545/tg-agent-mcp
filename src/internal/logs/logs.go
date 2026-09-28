// Package logs собирает логгер сервиса по контракту платформы.
//
// Контракт из platform.md: JSON в stdout и поля service и env в каждой записи.
// Пакет существует ради этих двух полей: без общего места каждый сервис
// называет их по-своему, и сводный поиск по логам перестаёт работать ровно
// тогда, когда он нужен - в инциденте на нескольких сервисах сразу.
package logs

import (
	"log/slog"
	"os"
)

// New собирает JSON-логгер и ставит его умолчанием процесса. Умолчание нужно
// потому, что не всякий код таскает логгер параметром, а запись мимо контракта
// теряется в общем поиске.
func New(service, environment string, level slog.Level) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("service", service, "env", environment)
	slog.SetDefault(logger)
	return logger
}
