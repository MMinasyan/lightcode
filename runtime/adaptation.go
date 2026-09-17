package runtime

import (
	"reflect"

	"github.com/MMinasyan/lightcode/model"
)

// Adaptation is the public per-model treatment value a ModelAdaptation
// resolves: which registered tools the model is shown, which coaching blocks
// join its system prompt, and which per-section additions and tool-description
// replacements apply. The zero value is a valid no-op baseline.
type Adaptation struct {
	ExcludeTools, IncludeTools, Blocks     []string
	Additions, ToolDescriptionReplacements map[string]string
}

// ModelAdaptation is the consumed public capability: one Resolve per
// preparation converts the active model ref into the treatment applied to the
// captured prompt and tool surface. Resolve returns only the value above and
// cannot add capability IDs, hooks, or plugin implementations.
type ModelAdaptation interface {
	Resolve(model.ModelRef) (Adaptation, error)
}

// modelAdaptationType is the declared-type identity of the public
// ModelAdaptation contract: composition admits exactly one Runtime-scoped
// export declared as this interface.
var modelAdaptationType = reflect.TypeFor[ModelAdaptation]()
