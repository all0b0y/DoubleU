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

// AIMemorySource selects which side owns memory writes after extraction.
type AIMemorySource string

const (
	// MemorySourceGo keeps the legacy flow: Go persists extracted facts and
	// profile suggestions returned by the Python service.
	MemorySourceGo AIMemorySource = "go"
	// MemorySourcePython delegates fact/suggestion persistence to the Python
	// pydantic-ai runtime (direct PostgreSQL access); Go skips its write path
	// so nothing is stored twice.
	MemorySourcePython AIMemorySource = "python"
)

// ReflectionMode controls sleep-time reflection in the nightly worker (#5).
type ReflectionMode string

const (
	// ReflectionOff disables reflection entirely (default, safe-off).
	ReflectionOff ReflectionMode = "off"
	// ReflectionShadow synthesises patterns and logs them but writes nothing —
	// for eval-driven validation before enabling writes.
	ReflectionShadow ReflectionMode = "shadow"
	// ReflectionEnabled writes synthesised behaviour_pattern facts to memory.
	ReflectionEnabled ReflectionMode = "enabled"
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
	// AIMemorySource picks who persists extracted memory (Go legacy path or
	// the Python agent runtime). Defaults to MemorySourceGo (safe-off).
	AIMemorySource AIMemorySource
	// ReflectionMode gates sleep-time reflection in the nightly worker.
	// Defaults to ReflectionOff (safe-off).
	ReflectionMode ReflectionMode
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
		AIMemorySource:      parseMemorySource(os.Getenv("AI_MEMORY_SOURCE")),
		ReflectionMode:      parseReflectionMode(os.Getenv("REFLECTION_MODE")),
	}
}

func parseReflectionMode(raw string) ReflectionMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(ReflectionShadow):
		return ReflectionShadow
	case string(ReflectionEnabled):
		return ReflectionEnabled
	default:
		return ReflectionOff
	}
}

func parseMemorySource(raw string) AIMemorySource {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(MemorySourcePython):
		return MemorySourcePython
	default:
		return MemorySourceGo
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
