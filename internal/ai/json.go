package ai

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrNotJSON: the model's answer held no JSON object.
var ErrNotJSON = errors.New("ai: the answer is not the requested JSON")

// DecodeJSON reads the JSON object a model was asked to answer with into v.
// Models wrap it in ```json fences or a sentence often enough that the outer
// text is cut away first: everything from the first '{' to the last '}'.
func DecodeJSON(answer string, v any) error {
	s := strings.TrimSpace(stripReasoning(answer))
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return ErrNotJSON
	}
	if err := json.Unmarshal([]byte(s[i:j+1]), v); err != nil {
		return errors.Join(ErrNotJSON, err)
	}
	return nil
}
