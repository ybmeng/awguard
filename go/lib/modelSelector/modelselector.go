// Package modelselector is the universal model registry: every bot references
// a model by ModelID, and this library owns what those IDs mean and how they
// route. Two parts per entry: the human-facing model name and the universal ID.
//
// v0 routes everything through OpenRouter; adding a provider or model is one
// line in the registry.
package modelselector

// ModelID is the universal model identifier: "provider/model-slug".
// e.g. "openrouter/deepseek/deepseek-v4". Stored in specs (botnet.Bot.Model),
// resolved here.
type ModelID string

// Model pairs the display name with the universal ID.
type Model struct {
	Name string  `json:"name"` // human-facing, e.g. "DeepSeek V4"
	ID   ModelID `json:"id"`
}

// The starting roster: OpenRouter routing between DeepSeek V4 and GLM 5.3 Flash.
var (
	DeepSeekV4 = Model{Name: "DeepSeek V4 Flash", ID: "openrouter/deepseek/deepseek-v4-flash-0731"}
	GLM53Flash = Model{Name: "GLM 5.3 Flash", ID: "openrouter/z-ai/glm-5.3-flash"}
)

var registry = []Model{DeepSeekV4, GLM53Flash}

// All returns the selectable models, in registry order.
func All() []Model {
	out := make([]Model, len(registry))
	copy(out, registry)
	return out
}

// ByID resolves a universal ID to its Model.
func ByID(id ModelID) (Model, bool) {
	for _, m := range registry {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
