// OpenAI Codex (ChatGPT-subscription) constants, sourced from the openai/codex
// client (codex-rs/model-provider-info/src/lib.rs, codex-rs/login/src/auth/manager.rs).
package llm

// codexBaseURL is the ChatGPT-subscription Responses endpoint; the SDK
// appends "/responses".
const codexBaseURL = "https://chatgpt.com/backend-api/codex"

// oauthClientID is Codex CLI's public OAuth client id (login refresh flow).
const oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// refreshTokenURL is OpenAI's OAuth token endpoint.
var refreshTokenURL = "https://auth.openai.com/oauth/token"
