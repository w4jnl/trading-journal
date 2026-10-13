package ai

import "time"

// Preset is a hosted provider a user can pick in Settings to run the reviews
// on their own API key. The endpoint is fixed here, never typed by the user, so
// no one can point the server at a host on its own network; the free-form
// OpenAI-compatible URL (ProviderCustom) is for admins only.
type Preset struct {
	ID      string // stored in user_ai_settings.provider
	Label   string
	Dialect string // the Options.Provider the client speaks: "openai" or "anthropic"
	BaseURL string
	KeyURL  string // where the user creates a key
	// CompletionTokens sends max_completion_tokens (OpenAI's reasoning models
	// reject max_tokens).
	CompletionTokens bool
	Headers          map[string]string
}

// ProviderCustom is the admin-only choice of any OpenAI-compatible endpoint (a
// local model server, Groq, DeepSeek, ...). Its key is optional.
const ProviderCustom = "custom"

// Presets are the hosted providers offered in Settings, in display order.
var Presets = []Preset{
	{ID: "openai", Label: "OpenAI", Dialect: "openai", BaseURL: "https://api.openai.com/v1",
		KeyURL: "https://platform.openai.com/api-keys", CompletionTokens: true},
	{ID: "anthropic", Label: "Anthropic (Claude)", Dialect: "anthropic", BaseURL: "https://api.anthropic.com/v1",
		KeyURL: "https://platform.claude.com/settings/keys"},
	{ID: "gemini", Label: "Google Gemini", Dialect: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		KeyURL: "https://aistudio.google.com/apikey"},
	{ID: "mistral", Label: "Mistral", Dialect: "openai", BaseURL: "https://api.mistral.ai/v1",
		KeyURL: "https://console.mistral.ai/api-keys"},
	{ID: "openrouter", Label: "OpenRouter", Dialect: "openai", BaseURL: "https://openrouter.ai/api/v1",
		KeyURL: "https://openrouter.ai/settings/keys", Headers: map[string]string{"X-Title": "Trading Journal"}},
}

// PresetByID returns the hosted preset with that id.
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// CustomPreset is the OpenAI-compatible endpoint at baseURL.
func CustomPreset(baseURL string) Preset {
	return Preset{ID: ProviderCustom, Label: "Custom (OpenAI-compatible)", Dialect: "openai", BaseURL: baseURL}
}

// Per-user client limits. A hosted reasoning model spends part of the output
// budget thinking before it writes, and only the tokens actually produced are
// billed, so the budget is generous; the timeout allows for that thinking.
const (
	userMaxTokens = 8192
	userTimeout   = 180 * time.Second
)

// Options builds the client options for this provider with the user's model
// and key. The provider keeps its default temperature: several hosted
// reasoning models reject any other value.
func (p Preset) Options(model, apiKey string) Options {
	return Options{
		Provider:         p.Dialect,
		BaseURL:          p.BaseURL,
		Model:            model,
		APIKey:           apiKey,
		Timeout:          userTimeout,
		MaxTokens:        userMaxTokens,
		Headers:          p.Headers,
		OmitTemperature:  true,
		CompletionTokens: p.CompletionTokens,
	}
}
