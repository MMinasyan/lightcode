package melta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/protocol"
)

// commandsHandler returns the fixed melta.commands/v1 host service method. The
// bound Session, target Workspace and request identity come only from the
// host-issued CallContext; the arguments carry the method's ordinary generated
// protocol value. Only a session_work call holds the command grant.
func (c *composition) commandsHandler(method string) (extension.Handler, bool) {
	switch method {
	case "create_session", "submit", "fork", "compact", "set_agent", "set_model",
		"interrupt", "stop", "archive", "reopen", "delete", "revert_code":
		return func(ctx context.Context, call extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
			return c.runCommand(ctx, call, method, arguments)
		}, true
	}
	return nil, false
}

// runCommand is the one fixed command bridge: it derives the ordinary Caller
// from the original grant and target, decodes the exact generated request
// value, and calls the existing public command producer. It never executes a
// second admission or command implementation.
func (c *composition) runCommand(ctx context.Context, call extension.CallContext, method string, arguments json.RawMessage) (extension.Result, error) {
	grant := hostGrant(ctx)
	if !grant.commands || grant.commandScope.Kind == "" {
		return extension.Result{}, fmt.Errorf("command binding %q is not granted: %w", method, ErrDenied)
	}
	if c.hosts == nil || c.hosts.owner == nil {
		return extension.Result{}, ErrClosed
	}
	owner := c.hosts.owner
	caller, err := owner.caller(CallerContext{Origin: harness.InputOriginPlugin, ExtensionID: call.ExtensionID, Scope: grant.commandScope})
	if err != nil {
		return extension.Result{}, err
	}
	switch method {
	case "create_session":
		var request protocol.CreateSessionRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		result, err := owner.CreateSession(ctx, caller, request.Workspace, request.AgentSelection)
		return commandResult(result, err)
	case "submit":
		var request protocol.SubmitRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		result, err := owner.SubmitSession(ctx, caller, sessionID, request)
		return commandResult(result, err)
	case "fork":
		var request protocol.ForkRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		result, err := owner.ForkSession(ctx, caller, sessionID, request)
		return commandResult(result, err)
	case "compact":
		var request protocol.CompactRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		result, err := owner.CompactSession(ctx, caller, sessionID, request)
		return commandResult(result, err)
	case "set_agent":
		var request protocol.SetSessionAgentTypeRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		result, err := owner.SetSessionAgentType(ctx, caller, sessionID, request)
		return commandResult(result, err)
	case "set_model":
		var request protocol.SetSessionModelRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		result, err := owner.SetSessionModel(ctx, caller, sessionID, request.Model)
		return commandResult(result, err)
	case "interrupt", "stop", "archive", "reopen", "delete":
		if err := decodeCommandValue(arguments, &struct{}{}); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		switch method {
		case "interrupt":
			err = owner.InterruptSession(ctx, caller, sessionID)
			return nullCommandResult(err)
		case "stop":
			err = owner.StopSession(ctx, caller, sessionID)
			return nullCommandResult(err)
		case "archive":
			result, err := owner.ArchiveSession(ctx, caller, sessionID)
			return commandResult(result, err)
		case "reopen":
			result, err := owner.ReopenSession(ctx, caller, sessionID)
			return commandResult(result, err)
		default:
			result, err := owner.DeleteSession(ctx, caller, sessionID)
			return commandResult(result, err)
		}
	case "revert_code":
		var request protocol.RevertCodeRequest
		if err := decodeCommandValue(arguments, &request); err != nil {
			return extension.Result{}, err
		}
		sessionID, err := boundSession(call)
		if err != nil {
			return extension.Result{}, err
		}
		result, err := owner.RevertSessionCode(ctx, caller, sessionID, request.BoundaryOperationId)
		if err != nil && result.Error == nil {
			return extension.Result{}, err
		}
		if err != nil {
			// The command's public error class stays available to the author
			// handler; the produced accumulated result travels beside it as the
			// typed call error's owned partial.
			partial, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				return extension.Result{}, marshalErr
			}
			return extension.Result{}, &extension.CallError{Err: err, Partial: partial}
		}
		return commandResult(result, nil)
	}
	return extension.Result{}, fmt.Errorf("command method %q is not declared: %w", method, harness.ErrInvalid)
}

// boundSession returns the host-issued Session identity; arguments can never
// supply it.
func boundSession(call extension.CallContext) (string, error) {
	if call.SessionID == "" {
		return "", fmt.Errorf("command requires a Session target: %w", harness.ErrInvalid)
	}
	return call.SessionID, nil
}

// decodeCommandValue applies the shared strict generated-value decoder to one
// command request value and wraps its refusal in the public invalid class
// before any producer mutates state.
func decodeCommandValue(arguments json.RawMessage, destination any) error {
	if err := protocol.DecodeValue(arguments, destination); err != nil {
		return fmt.Errorf("command arguments: %v: %w", err, harness.ErrInvalid)
	}
	return nil
}

// commandResult projects one ordinary generated command receipt.
func commandResult(value any, err error) (extension.Result, error) {
	if err != nil {
		return extension.Result{}, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: raw}, nil
}

// nullCommandResult is the existing null receipt of the argument-free controls.
func nullCommandResult(err error) (extension.Result, error) {
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Value: json.RawMessage("null")}, nil
}
