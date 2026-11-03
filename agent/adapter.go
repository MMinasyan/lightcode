package agent

import (
	"context"
	"fmt"

	"github.com/MMinasyan/lightcode/model"
)

// ToolFunc is one tool implementation in its plain standalone shape: the started call goes in, its settled result and error come out. AdaptTools is the only route a ToolFunc reaches a run through; the loop itself never sees this type.
type ToolFunc func(context.Context, model.ToolCall) (model.ToolResult, error)

// AdaptTools builds the ordinary standalone ToolEffect over a name-registered tool map. The map is copied at construction, so later caller mutations of the input map cannot reach the adapter; a nil or empty map is valid for a zero-tool run. Per started call the adapted effect consults, in order, the owned per-call advertised-name copy of the producing request that Run hands it, then the copied registry. One rule covers every miss: a call whose name is unadvertised, absent from the registry, or registered as a nil ToolFunc settles the ordinary unavailable-tool error result — the call never reaches any implementation, no nil function is ever invoked, and that error settlement leaves every later call of the batch eligible. Every other call forwards the registered function's result and error unchanged: settled-result validation belongs to Run's existing validator, not to the adapter.
func AdaptTools(tools map[string]ToolFunc) ToolEffect {
	registered := make(map[string]ToolFunc, len(tools))
	for name, fn := range tools {
		registered[name] = fn
	}
	return func(ctx context.Context, call model.ToolCall, names []string) (model.ToolResult, error) {
		advertised := false
		for _, name := range names {
			if name == call.Name {
				advertised = true
				break
			}
		}
		fn, ok := registered[call.Name]
		if !advertised || !ok || fn == nil { // one gate covers every miss — unadvertised, unregistered, or a nil registration: the call never reaches an implementation, and the ordinary unavailable settlement answers it.
			return model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: fmt.Sprintf("Tool %q is not available.", call.Name)}, nil
		}
		return fn(ctx, call) // result and error travel verbatim to Run's validator.
	}
}

// AdaptModelEffect builds the ordinary standalone ModelEffect over one model.Transport: exactly one physical Stream attempt with the request Run hands it and no per-call runtime extras, assembled with exactly the given source identity through Run's assembly callback. Before any dispatch, a nil transport or an incomplete source identity returns the typed boundary-protocol violation naming the "model" boundary. A pre-acceptance transport failure that is cancellation-shaped under a genuinely done run context settles the interruption disposition with the shared interruption detail — an active model effect never reports cancellation as a Go error — while every other transport failure settles the failure disposition carrying that error's text with no output. On an accepted stream the assembler's error is returned verbatim as a Go error, and the finalized output maps through the existing settlement algebra: completed becomes ready, errored becomes failure retaining the partial output and its detail, interrupted becomes interruption carrying its output and detail. The transport's protocol-warning diagnostics are discarded here because ModelEffect has no diagnostics channel; direct transport callers retain that API. The adapter owns no retry: one attempt per model effect.
func AdaptModelEffect(source model.ModelRef, t *model.Transport) ModelEffect {
	return func(ctx context.Context, req model.Request, cb AssemblyCallback) (ModelSettlement, error) {
		if t == nil || !nonzeroSource(source) {
			return ModelSettlement{}, newBoundaryViolation("model", "model effect adapter requires a non-nil transport and a complete source model identity")
		}
		stream, _, err := t.Stream(ctx, req, nil) // warnings discarded: they are diagnostics, not a ModelEffect channel.
		if err != nil {
			if isCancellation(err) && ctx.Err() != nil { // pre-acceptance cancellation is never ordinary provider failure.
				return ModelSettlement{Disposition: DispoInterruption, Detail: detailInterrupted}, nil
			}
			return ModelSettlement{Disposition: DispoFailure, Detail: err.Error()}, nil
		}
		out, err := cb(source, stream)
		if err != nil {
			_ = stream.Close()            // cleanup-only: a callback that rejects the stream never takes ownership, and the exactly-once idempotent Close makes this safe whether or not the assembler already released it.
			return ModelSettlement{}, err // assembler failures are preserved verbatim.
		}
		switch out.Status {
		case model.OutputCompleted:
			return ModelSettlement{Disposition: DispoReady, Output: &out}, nil
		case model.OutputErrored:
			return ModelSettlement{Disposition: DispoFailure, Output: &out, Detail: out.Detail}, nil
		default: // OutputInterrupted: the only remaining closed status.
			return ModelSettlement{Disposition: DispoInterruption, Output: &out, Detail: out.Detail}, nil
		}
	}
}
