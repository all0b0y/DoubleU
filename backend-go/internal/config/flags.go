package config

import (
	"os"
	"strings"
)

// LLMGatewayKind selects the egress path for chat-completion calls.
type LLMGatewayKind string

const (
	// GatewayNone keeps the legacy direct-to-provider path.
	GatewayNone LLMGatewayKind = "none"
	// GatewayLiteLLM routes every completion through LiteLLM Proxy.
	GatewayLiteLLM LLMGatewayKind = "litellm"
)

// ObservabilitySink selects the trace destination.
type ObservabilitySink string

const (
	// SinkNone disables observability (no Langfuse, no OTel exporter).
	SinkNone ObservabilitySink = "none"
	// SinkLangfuse sends OTel spans to a self-hosted Langfuse endpoint.
	SinkLangfuse ObservabilitySink = "langfuse"
)

// FeatureFlags toggles the LLM Safety & Observability pipeline stages.
//
// Every flag defaults safe-off so production behaviour is unchanged until an
// operator explicitly opts in via environment variables.
type FeatureFlags struct {
	// SafetyInputEnabled gates the Phase 1 InputSafetyScanner inside ModelRouter.
	SafetyInputEnabled bool
	// SafetyOutputEnabled gates the Phase 3 OutputSafetyScanner inside ModelRouter.
	SafetyOutputEnabled bool
	// LLMGateway selects the Phase 2 gateway.  GatewayNone keeps legacy path.
	LLMGateway LLMGatewayKind
	// ObservabilitySink selects the Phase 4 trace destination.
	ObservabilitySink ObservabilitySink
	// ShadowModeOnly runs both legacy and LiteLLM gateways but returns legacy.
	ShadowModeOnly bool
	// Cutover skips the legacy gateway entirely and routes only via LiteLLM.
	Cutover bool
}

// LoadFlags reads FeatureFlags from environment variables with safe defaults.
func LoadFlags() FeatureFlags {
	return FeatureFlags{
		SafetyInputEnabled:  boolValue(os.Getenv("SAFETY_INPUT_ENABLED")),
		SafetyOutputEnabled: boolValue(os.Getenv("SAFETY_OUTPUT_ENABLED")),
		LLMGateway:          parseGateway(os.Getenv("LLM_GATEWAY")),
		ObservabilitySink:   parseSink(os.Getenv("OBSERVABILITY_SINK")),
		ShadowModeOnly:      boolValue(os.Getenv("SHADOW_MODE_ONLY")),
		Cutover:             boolValue(os.Getenv("CUTOVER")),
	}
}

func parseGateway(raw string) LLMGatewayKind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(GatewayLiteLLM):
		return GatewayLiteLLM
	default:
		return GatewayNone
	}
}

func parseSink(raw string) ObservabilitySink {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(SinkLangfuse):
		return SinkLangfuse
	default:
		return SinkNone
	}
}
