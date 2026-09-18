// Package middleware provides HTTP middleware for the ILTER-AI proxy.
//
// Middleware Chain Order (for /v1/chat/completions requests):
//
//  1. ObservabilityHandler (optional, OTel metrics)
//  2. AuthMiddleware         - validates API key, sets key_id in context
//  3. RateLimitMiddleware    - enforces RPM/TPM limits per API key
//  4. BudgetMiddleware       - enforces monthly budget per API key
//  5. PromptInjection        - injects configured system prompts
//  6. PIIMaskerMiddleware    - masks credit cards, TC Kimlik, SSN, email, phone, IPs
//  7. GuardrailsMiddleware   - prompt-injection, toxicity, topic-block checks
//  8. MCPInjectMiddleware    - optionally injects authorized MCP tools into
//     requests and runs the transparent tool-call loop. Injection is a per-key
//     opt-in (see api_keys.mcp_injection_enabled). Interception happens only
//     when Ilter itself injected tools this request or the Ilter tool sentinel
//     is present in message history; client-sent tools and client-replayed
//     tool results always pass through untouched. Executes tools via MCP and
//     returns the final result.
//  9. SmartRouterMiddleware  - reads active strategy, scores, matches rules,
//     selects model & provider preference, stores in context. The raw
//     requested model (possibly "provider/model") is stored in StrategyKey;
//     the ChatCompletions handler resolves provider pinning against the
//     load balancer (internal/proxy/handler.go resolveRequestedModel).
//
// 10. LoopDetectorMiddleware - detects and breaks request loops
//
// 11. SemanticCacheMiddleware - checks cache via embedding
//
// 12. ChatCompletions handler- routes to provider, handles streaming, records audit+budget
//
// This ordering ensures that auth and rate-limiting happen before any PII processing,
// PII and guardrails run before MCP injection, and the semantic cache sees the
// fully-enriched request (with injected tools).
package middleware
