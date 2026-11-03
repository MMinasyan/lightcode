package harness

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"

	"github.com/MMinasyan/lightcode/model"
)

// The context strategy contract: one pure algorithm deciding the context
// maintenance of a conversation. Plan and Consume are pure — no context, no
// I/O, no dispatch, no canonical mutation. The selected strategy's identity
// and declared requirements are resolved execution inputs captured on the
// ExecutionCapture; its implementation is an owned execution resource on the
// opened Execution.

// Trigger kinds of one maintenance procedure, the closed PlanInput set.
const (
	triggerAutomatic = "automatic"
	triggerManual    = "manual"
)

// PlanInput is the frozen owned input of one maintenance procedure: the
// trigger kind, the frozen canonical model messages of the conversation view
// the procedure may consume, that view's canonical checkpoint and boundary
// references, the captured instructions and tool catalog, the separate frozen
// request contributions, the optional conversation measurement, the captured
// model limits, and the captured strategy settings as owned strategy input.
type PlanInput struct {
	Trigger           string
	Messages          []model.Message
	CheckpointEntryID string
	BoundaryEntryID   string
	Instructions      string
	Tools             []model.ToolDefinition
	Contributions     *RequestEvidence
	Measurement       *UsageCount
	ContextWindow     int
	OutputReserve     int
	Settings          json.RawMessage
}

// ConsumeInput is the completed-model feedback of one model plan: the same
// frozen input the plan consumed, the plan's raw local State, and the
// completed model output. State is process-local only and is never durable.
type ConsumeInput struct {
	Input  PlanInput
	State  json.RawMessage
	Output model.Output
}

// PlanKind is the closed discriminator of one PlanOutput.
type PlanKind string

const (
	PlanNone       PlanKind = "none"
	PlanModel      PlanKind = "model"
	PlanCheckpoint PlanKind = "checkpoint"
)

// ModelPlan is one tool-free maintenance model request plus the raw local
// State threading through to the Consume that follows its completed output.
type ModelPlan struct {
	Request model.Request
	State   json.RawMessage
}

// CheckpointItem is one ordered checkpoint proposal item: exactly one of a
// Retain source position or an explicitly strategy-authored Context message.
// Retain is a zero-based position in the procedure's frozen canonical
// PlanInput.Messages (zero is valid), never in request contributions; only a
// retained source may carry a real tool exchange. Context is authored context
// that passes the shared message validation with no tool calls or tool-result
// call id. A malformed item fails before any commit.
type CheckpointItem struct {
	Retain  *int
	Context *model.Message
}

// CheckpointPlan is one proposed context checkpoint: the covered canonical
// boundary and the ordered nonempty item list defining the new model-visible
// context. Proposal positions are never durable lineage; the Harness
// materializes accepted items into self-contained checkpoint messages.
type CheckpointPlan struct {
	BoundaryEntryID string
	Items           []CheckpointItem
}

// PlanOutput is the closed plan union: exactly one of none, model, or
// checkpoint. A value whose discriminator and variants disagree is malformed
// and fails the procedure before any commit.
type PlanOutput struct {
	Kind       PlanKind
	Model      *ModelPlan
	Checkpoint *CheckpointPlan
}

// ContextStrategy is the pure context-maintenance algorithm. Plan decides the
// next step over the frozen input; Consume folds one completed model output
// back into the plan stream. Neither callback may dispatch effects or mutate
// canonical state.
type ContextStrategy interface {
	Plan(PlanInput) (PlanOutput, error)
	Consume(ConsumeInput) (PlanOutput, error)
}

// StrategyBinding is one selected context strategy binding: its stable
// identity, whether it requires a known conversation context window before
// admission, and the owned implementation. The included binding is fixed.
type StrategyBinding struct {
	ID                    string
	RequiresContextWindow bool
	Strategy              ContextStrategy
}

// IncludedStrategyID is the included summary policy's fixed identity.
const IncludedStrategyID = "melta.summary/v1"

// IncludedStrategyBinding returns the included summary policy binding: the
// current pressure-triggered single-summary algorithm over the captured
// maintenance model and prompt, requiring a known context window.
func IncludedStrategyBinding() *StrategyBinding {
	return &StrategyBinding{
		ID:                    IncludedStrategyID,
		RequiresContextWindow: true,
		Strategy:              summaryStrategy{},
	}
}

// resolveStrategyBinding selects the execution's binding: nil is the included
// one.
func resolveStrategyBinding(binding *StrategyBinding) *StrategyBinding {
	if binding == nil {
		return IncludedStrategyBinding()
	}
	return binding
}

// validatePlanOutput enforces the closed plan union: exactly one variant
// beside its discriminator, a tool-free model request, and well-formed raw
// state. Checkpoint item exclusivity, positions and authored-message rules are
// owned by resolveCheckpointItems, which validates every proposal against the
// frozen view before publication.
func validatePlanOutput(plan PlanOutput) error {
	switch plan.Kind {
	case PlanNone:
		if plan.Model != nil || plan.Checkpoint != nil {
			return errors.New("none plan carries a model or checkpoint variant")
		}
	case PlanModel:
		if plan.Model == nil || plan.Checkpoint != nil {
			return errors.New("model plan requires exactly its model variant")
		}
		if len(plan.Model.Request.Tools) != 0 { // tool-free: the nil and empty slices alike
			return errors.New("maintenance model request is tool-free")
		}
		if len(plan.Model.State) > 0 && !json.Valid(plan.Model.State) {
			return errors.New("maintenance plan state must be valid JSON")
		}
	case PlanCheckpoint:
		if plan.Model != nil || plan.Checkpoint == nil {
			return errors.New("checkpoint plan requires exactly its checkpoint variant")
		}
		if plan.Checkpoint.BoundaryEntryID == "" {
			return errors.New("checkpoint plan requires its covered boundary")
		}
		if len(plan.Checkpoint.Items) == 0 {
			return errors.New("checkpoint plan items must not be empty")
		}
	default:
		return errors.New("plan kind " + string(plan.Kind) + " is outside the closed union")
	}
	return nil
}

// summaryStrategy is the included context policy: the current
// pressure-triggered single-summary algorithm. Its automatic trigger computes
// the exact signed pressure total over the captured measurement and limits;
// its manual trigger always plans the one summary request. Consume requires
// the completed nonempty text summary and folds it into the one summary
// checkpoint message. It is pure and holds no state.
type summaryStrategy struct{}

func (summaryStrategy) Plan(in PlanInput) (PlanOutput, error) {
	if in.Trigger == triggerAutomatic {
		if in.Measurement == nil { // a missing report never overflows
			return PlanOutput{Kind: PlanNone}, nil
		}
		total := new(big.Int).SetInt64(in.Measurement.InputTokens)
		total.Add(total, big.NewInt(in.Measurement.CachedInputTokens))
		total.Add(total, big.NewInt(in.Measurement.OutputTokens))
		threshold := big.NewInt(int64(in.ContextWindow) - int64(in.OutputReserve))
		if total.Cmp(threshold) <= 0 {
			return PlanOutput{Kind: PlanNone}, nil
		}
	}
	request, err := summaryRequest(in)
	if err != nil {
		return PlanOutput{}, err
	}
	return PlanOutput{Kind: PlanModel, Model: &ModelPlan{Request: request}}, nil
}

func (summaryStrategy) Consume(in ConsumeInput) (PlanOutput, error) {
	summary := ""
	if in.Output.Message != nil {
		summary = in.Output.Message.TextContent()
	}
	if in.Output.Status != model.OutputCompleted || summary == "" {
		// A textless completed summary — including refusal-only — and a
		// failed output are algorithm failures.
		return PlanOutput{}, errors.New("compaction summary is empty")
	}
	msg, err := model.NewMessage(model.Message{
		Role:    model.RoleAssistant,
		Source:  in.Output.Source,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "[Previous conversation summary]\n\n" + summary + "\n\n[End of summary. Continue from here.]"}},
	})
	if err != nil {
		return PlanOutput{}, err
	}
	return PlanOutput{Kind: PlanCheckpoint, Checkpoint: &CheckpointPlan{
		BoundaryEntryID: in.Input.BoundaryEntryID, // the current boundary rule over the frozen input
		Items:           []CheckpointItem{{Context: &msg}},
	}}, nil
}

// summaryRequest renders the included policy's one full-input summary
// request: the captured instructions as the system message and the whole
// frozen conversation serialized into inert transcript lines joined as one
// user message. An empty snapshot is the retained nothing-to-compact failure.
func summaryRequest(in PlanInput) (model.Request, error) {
	if len(in.Messages) == 0 {
		return model.Request{}, errors.New("nothing to compact")
	}
	lines, err := serializeCompactionMessages(in.Messages)
	if err != nil {
		return model.Request{}, err
	}
	return model.Request{Messages: []model.Message{
		{Role: model.RoleSystem, Content: []model.ContentPart{{Kind: model.PartText, Text: in.Instructions}}},
		{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: strings.Join(lines, "\n")}}},
	}}, nil
}
