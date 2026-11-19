package actions

import "github.com/Autarquico/codeflock/internal/domain"

// HintFor exposes hintFor to the tests.
var HintFor = func(code domain.Code, details map[string]any) string { return hintFor(code, details) }
