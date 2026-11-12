package melta

// This file owns the one post-commit local/external cleanup producer used by
// direct Session deletion and the lifecycle sweep. Canonical deletion is
// already committed; every local tree removal and every recorded external
// owner is attempted, individual failures are reported, and nothing is ever
// rolled back. External cleanup is an ordinary guarded Harness-scoped service
// call with no Session binding or grant.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Syntheresis/melta/protocol"
)

// cleanupUnavailableMessage is the fixed non-secret external failure detail:
// guest error text and domain payloads never enter the public result.
const cleanupUnavailableMessage = "extension data cleanup is unavailable"

// cleanupCallDeadline bounds one cooperative external cleanup callback. It is
// a cancellation opportunity, never forced termination.
const cleanupCallDeadline = 5 * time.Second

// deletedSession is one committed deletion's local identity and its
// once-returned external-data owners.
type deletedSession struct {
	sessionID string
	owners    []string
}

// cleanupDeletedSessions runs the shared post-commit producer: it always
// removes every committed Session tree, then takes one current
// configuration/resource reservation for the whole owner batch and invokes
// each owner's cleanup declaration. Every failure is reported separately;
// one failing owner never suppresses a sibling, local failure never suppresses
// external cleanup, and only the first result can report owners.
func (r *Melta) cleanupDeletedSessions(ctx context.Context, deletions []deletedSession) []protocol.CleanupFailure {
	failures := make([]protocol.CleanupFailure, 0)
	for _, deletion := range deletions {
		if err := r.removeSessionCode(deletion.sessionID); err != nil {
			failures = append(failures, protocol.CleanupFailure{Code: "storage", Message: err.Error()})
		}
	}
	hasOwners := false
	for _, deletion := range deletions {
		if len(deletion.owners) != 0 {
			hasOwners = true
			break
		}
	}
	if !hasOwners {
		return failures
	}
	captured, err := r.config.captureFor(ctx, true)
	snapshot := captured.snapshot
	if err != nil || snapshot == nil || snapshot.resources == nil {
		return appendOwnerFailures(failures, deletions)
	}
	defer func() {
		_ = snapshot.resources.release()
	}()
	for _, deletion := range deletions {
		for _, owner := range deletion.owners {
			if failure := r.cleanupExternalOwner(ctx, snapshot, deletion.sessionID, owner); failure != nil {
				failures = append(failures, *failure)
			}
		}
	}
	return failures
}

// appendOwnerFailures marks every recorded owner unavailable when the current
// resource batch cannot be observed at all.
func appendOwnerFailures(failures []protocol.CleanupFailure, deletions []deletedSession) []protocol.CleanupFailure {
	for _, deletion := range deletions {
		for _, owner := range deletion.owners {
			extensionID := owner
			failures = append(failures, protocol.CleanupFailure{ExtensionId: &extensionID, Code: "unavailable", Message: cleanupUnavailableMessage})
		}
	}
	return failures
}

// cleanupExternalOwner invokes one recorded owner's declared cleanup method
// through the ordinary guarded provider guard. A disabled, uninstalled or
// missing provider/capability and a failed call are the same unavailable
// outcome; the deleted identity is an argument only, never Session binding.
func (r *Melta) cleanupExternalOwner(ctx context.Context, snapshot *configuration, sessionID, owner string) *protocol.CleanupFailure {
	extensionID := owner
	failure := &protocol.CleanupFailure{ExtensionId: &extensionID, Code: "unavailable", Message: cleanupUnavailableMessage}
	composition := snapshot.composition
	if composition == nil || composition.compiled == nil || snapshot.resources.runtime == nil {
		return failure
	}
	capability, ok := composition.compiled.CleanupCapability(owner)
	if !ok {
		return failure
	}
	provider := snapshot.resources.runtime.capabilities[capability.ID]
	if provider == nil {
		return failure
	}
	arguments, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
	}{SessionID: sessionID})
	if err != nil {
		return failure
	}
	callCtx, cancel := context.WithTimeout(ctx, cleanupCallDeadline)
	defer cancel()
	result, err := composition.invokeProvider(callCtx, provider, capability.Methods, "cleanup", arguments, nil, callIdentity{revision: snapshot.resources.revisionString()}, false)
	if err != nil || result.Failure != nil {
		return failure
	}
	return nil
}

// cleanupFailureText renders one reported failure for the sweep's retained
// per-pass diagnostic.
func cleanupFailureText(failure protocol.CleanupFailure) string {
	if failure.ExtensionId != nil {
		return string(failure.Code) + ": " + failure.Message + " (extension " + *failure.ExtensionId + ")"
	}
	return string(failure.Code) + ": " + failure.Message
}
