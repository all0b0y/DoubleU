# LLM Gateway Cutover Runbook

This runbook drives the per-purpose migration from the legacy
`OpenRouterLLMGateway` to LiteLLM + the Phase 1/3 safety scanners.

## Flag semantics

| `SHADOW_MODE_ONLY` | `CUTOVER` | Behaviour                                                  |
| ------------------ | --------- | ---------------------------------------------------------- |
| `true`             | `false`   | Both gateways run; **legacy** result returned.            |
| `false`            | `false`   | Both gateways run; **LiteLLM** result returned.           |
| `*`                | `true`    | Only LiteLLM runs. Legacy is short-circuited.             |

`shadow_mode_only` defaults to `false`; `cutover` defaults to `false`.

## Per-purpose checklist

For each purpose (`embed`, `extract`, `summarize`, `draft`) in this order:

1. Bring up LiteLLM and confirm `/health/liveliness` is 200.
2. Set `LLM_GATEWAY=litellm` + `SHADOW_MODE_ONLY=true` for one purpose at a time.
   Legacy continues to serve users; LiteLLM produces shadow traffic only.
3. Watch Langfuse for 24 h:
   - divergence rate per purpose < the threshold below
   - p95 latency within +50 ms of the Phase 0 baseline
   - cost dashboard sane
4. Flip `SHADOW_MODE_ONLY=false`. LiteLLM now serves users; legacy keeps
   shadowing.  Watch the same dashboards another 24 h.
5. Set `CUTOVER=true` for that purpose.  Legacy stops running.
6. After every purpose has cut over: delete `OpenRouterLLMGateway` in a final
   PR (no live traffic depends on it).

## Divergence thresholds

| Purpose     | Score metric            | Threshold (`divergent` rate) |
| ----------- | ----------------------- | --------------------------- |
| `draft`     | edit-similarity         | < 0.5 %                     |
| `extract`   | Jaccard on `fact_text`  | < 0.5 %                     |
| `summarize` | ROUGE-L / edit proxy    | < 0.5 %                     |
| `embed`     | text identity           | < 0.1 %                     |

## Killswitch

If divergence spikes, revert by flipping `LLM_GATEWAY=none`.  Legacy direct
path resumes immediately; nothing else needs to roll back because the safety
layer + observability stay running on either path.
