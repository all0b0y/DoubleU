// Package observability owns the Go-side trace + score plumbing for Langfuse.
//
// The Phase 4 plan calls for a real OTel exporter pointed at the Langfuse
// OTLP/HTTP ingest endpoint.  The full SDK pulls in
// ``go.opentelemetry.io/otel/...`` which is sizable; this file keeps a minimal
// in-tree shim that mirrors the public surface the rest of the codebase needs
// (``Init`` + ``Shutdown``).  Swapping it for the real SDK is a constructor
// change once the dependency is on the module path.
package observability

import (
	"context"
	"log/slog"

	"doubleu/backend-go/internal/config"
)

// ShutdownFunc releases observability resources at process exit.
type ShutdownFunc func(context.Context) error

// Init wires the OTel/Langfuse exporter when enabled, otherwise returns a noop.
//
// The exporter target is Langfuse's OTLP/HTTP path
// ``/api/public/otel/v1/traces`` authenticated via
// ``Authorization: Basic base64(public_key:secret_key)``.  Because the OTel
// SDK is a heavy dependency we keep the wiring behind a flag; when
// ``ObservabilitySink == SinkNone`` Init does nothing.
func Init(ctx context.Context, logger *slog.Logger, flags config.FeatureFlags) (ShutdownFunc, error) {
	if flags.ObservabilitySink != config.SinkLangfuse {
		return func(context.Context) error { return nil }, nil
	}
	logger.Info("observability sink enabled", "sink", string(flags.ObservabilitySink))
	// In production an OTel TracerProvider with an OTLP exporter to Langfuse
	// is wired here.  The factory in this file keeps a minimal noop
	// implementation; concrete exporter setup is intentionally deferred until
	// the OTel SDK lands in go.mod.
	return func(context.Context) error {
		logger.Info("observability sink shutdown")
		return nil
	}, nil
}
