package mapper

import (
	"strings"

	"github.com/doitintl/litellm-datahub-exporter/internal/config"
	"github.com/doitintl/litellm-datahub-exporter/internal/datahub"
	"github.com/doitintl/litellm-datahub-exporter/internal/litellm"
)

// genaiDimensions emits the DoiT GenAI-intelligence system-label taxonomy
// (genai/*) so LiteLLM spend participates in the GenAI lens alongside the
// native provider integrations. Key set and value conventions follow DoiT's
// existing emitters (Databricks, Azure AI, Anthropic spend-report template):
// booleans are the strings "true"/"false", consumption is PAYG for
// per-token computed pricing, and model_family comes from a keyword table.
func genaiDimensions(model, userID, userEmail, keyAlias, feature string) []datahub.Dimension {
	return []datahub.Dimension{
		{Key: "genai/genai_spend", Type: "system_label", Value: "true"},
		{Key: "genai/model", Type: "system_label", Value: bareModel(model)},
		{Key: "genai/model_family", Type: "system_label", Value: modelFamily(model)},
		{Key: "genai/is_model_serving", Type: "system_label", Value: "true"},
		{Key: "genai/consumption_model", Type: "system_label", Value: "PAYG"},
		{Key: "genai/user_id", Type: "system_label", Value: userID},
		{Key: "genai/user_email", Type: "system_label", Value: userEmail},
		{Key: "genai/api_key_name", Type: "system_label", Value: keyAlias},
		{Key: "genai/feature", Type: "system_label", Value: feature},
	}
}

// bareModel strips LiteLLM's provider prefix ("anthropic/claude-…" → "claude-…").
func bareModel(model string) string {
	if _, rest, found := strings.Cut(model, "/"); found {
		return rest
	}

	return model
}

var modelFamilies = []struct{ keyword, family string }{
	{"claude", "Claude"},
	{"llama", "Meta Llama"},
	{"gpt-oss", "GPT OSS"},
	{"gpt", "GPT"},
	{"o1", "GPT"},
	{"o3", "GPT"},
	{"mixtral", "Mixtral"},
	{"codestral", "Mistral"},
	{"mistral", "Mistral"},
	{"gemini", "Gemini"},
	{"gemma", "Gemma"},
	{"deepseek", "DeepSeek"},
	{"qwen", "Qwen"},
	{"command", "Cohere"},
	{"cohere", "Cohere"},
	{"grok", "Grok"},
	{"nova", "Amazon Nova"},
	{"titan", "Amazon Titan"},
	{"phi", "Phi"},
	{"embed", "Embedding"},
}

func modelFamily(model string) string {
	m := strings.ToLower(bareModel(model))

	for _, f := range modelFamilies {
		if strings.Contains(m, f.keyword) {
			return f.family
		}
	}

	return "Custom Model"
}

// userEmail resolves genai/user_email, in this order:
//
//  1. user_api_key_user_email, which LiteLLM derives from the user record
//     linked to the virtual key. It always wins: it is LiteLLM's own answer.
//  2. the field named by GENAI_USER_EMAIL_SOURCE, and only when that value is
//     shaped like an email address.
//  3. empty, which is the default and drops the dimension.
//
// Step 2 exists for a deployment behind ONE shared virtual key, where step 1 is
// the same for every caller or empty. Step 2 is off unless declared, so an
// existing deployment emits exactly what it emitted before.
func userEmail(r litellm.SpendRow, source config.UserEmailSource) string {
	if r.Metadata.UserAPIKeyUserEmail != "" {
		return r.Metadata.UserAPIKeyUserEmail
	}

	var candidate string

	switch source {
	case config.UserEmailSourceEndUser:
		candidate = r.EndUser
	case config.UserEmailSourceKeyAlias:
		candidate = r.Metadata.UserAPIKeyAlias
	default:
		return ""
	}

	return emailOrEmpty(candidate)
}

// emailOrEmpty returns s only when it is shaped like an email address.
//
// It guards a DECLARED source against a deployment that turns the setting on
// and then sends opaque ids. The values reach dashboards where nothing marks
// one as inferred, so an id in an email field is worse than an empty field.
func emailOrEmpty(s string) string {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return ""
	}

	if strings.ContainsAny(s, " \t") || strings.Count(s, "@") != 1 {
		return ""
	}

	if !strings.Contains(s[at+1:], ".") {
		return ""
	}

	return s
}
