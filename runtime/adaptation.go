package runtime

import (
	"reflect"

	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
)

// Adaptation is the public per-model treatment value a ModelAdaptation
// resolves: which registered tools the model is hidden from, ordered prompt
// append paragraphs, and tool-description replacements. The zero value is a
// valid no-op baseline.
type Adaptation struct {
	ExcludeTools                []string
	Append                      []string
	ToolDescriptionReplacements map[string]string
}

// ModelAdaptation is the consumed public capability: one Resolve per
// preparation converts the active model ref and the preparation's prompt mode
// into the treatment applied to the captured prompt and tool surface. Resolve
// returns only the value above and cannot add capability IDs, hooks, or plugin
// implementations.
type ModelAdaptation interface {
	Resolve(model.ModelRef, prompt.Mode) (Adaptation, error)
}

// modelAdaptationType is the declared-type identity of the public
// ModelAdaptation contract: composition admits exactly one Runtime-scoped
// export declared as this interface.
var modelAdaptationType = reflect.TypeFor[ModelAdaptation]()
