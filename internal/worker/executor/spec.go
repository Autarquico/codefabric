package executor

import "encoding/json"

// specSchema is the layout of the frozen spec this executor reads.
const specSchema = 2

// specView is the part of the frozen spec the Worker acts on.
type specView struct {
	Schema       int    `json:"schema"`
	Prompt       string `json:"prompt"`
	Mode         string `json:"mode"`
	Requirements struct {
		Harness string `json:"harness"`
	} `json:"requirements"`
}

// parseSpec reads the spec of an assignment. A spec that cannot be read has
// schema 0, which is refused as an incompatible version.
func parseSpec(raw []byte) specView {
	var s specView
	if err := json.Unmarshal(raw, &s); err != nil {
		return specView{}
	}
	return s
}
