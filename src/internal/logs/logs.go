// Package logs builds the service logger according to the platform contract.
//
// The contract: JSON to stdout and the fields service and env in every record.
// The package exists for these two fields: without one shared place every
// service names them its own way, and the combined log search stops working
// exactly when it is needed - in an incident across several services at once.
package logs

import (
	"log/slog"
	"os"
)

// New builds a JSON logger and makes it the process default. The default is
// needed because not all code passes a logger as a parameter, and a record
// outside the contract is lost in the combined search.
func New(service, environment string, level slog.Level) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("service", service, "env", environment)
	slog.SetDefault(logger)
	return logger
}
