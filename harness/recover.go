package harness

import (
	"context"

	"github.com/MMinasyan/lightcode/model"
)

// recoveryInterruptedDetail is the exact terminal detail of every Operation
// Recover settles: process loss interrupted it.
const recoveryInterruptedDetail = "Operation interrupted by Runtime loss."

// Recover runs quiescent restart recovery against one Storage before live
// Harness construction: it enumerates the Session IDs, validates each graph
// through the same validator as materialization, and handles Sessions
// independently — a corrupt or malformed Session is left unchanged and does
// not block valid siblings (later materialization returns its corruption
// error), while any storage-service failure aborts the call. For each valid
// running Operation one transaction re-reads and decodes the current
// registers and performs the complete terminal interruption through the same
// commitTerminalSettlement helper live settlement uses: an interrupted tool
// result under every unresolved reservation, exactly one interruption
// signal, the Operation settlement consuming an active model effect's
// reserved identity when no assistant payload exists (process loss retained
// none) or a fresh identity otherwise, the terminal Operation, and the
// cleared Session current Operation. It starts no Agent, preparation, model,
// tool, hook, child, Job, or replacement admission, and performs no replay,
// repair, or truncation; a second run finds no running Operation and writes
// nothing.
func Recover(ctx context.Context, store Storage) error {
	if store == nil {
		return invalidInput("recovery requires non-nil storage")
	}
	ids, err := store.ListSessionIDs(ctx)
	if err != nil {
		return err
	}
	for _, sessionID := range ids {
		if err := validateHexID(sessionID, "session id"); err != nil { // a malformed routing identity has no valid owning Session and stays caller-invalid: its row is left untouched, the pass continues
			continue
		}
		graph, err := validateSessionGraph(ctx, store, sessionID)
		if err != nil {
			if !isCorruption(err) { // a corrupt Session is left unchanged and does not block valid siblings; every other error aborts the call
				return err
			}
			continue
		}
		for i := range graph.Operations {
			op := graph.Operations[i]
			if op.State.Status != OperationRunning {
				continue
			}
			if err := recoverRunningOperation(ctx, store, sessionID, op.Admission.OperationID); err != nil {
				if !isCorruption(err) { // a storage-service failure aborts the call
					return err
				}
				// a corruption discovered under the validated view leaves the Session unchanged
			}
		}
	}
	return nil
}

// recoverRunningOperation repairs one valid running Operation of a quiescent
// Session: its own transaction re-reads and decodes the current registers and
// performs the complete terminal interruption through the shared helper — no
// temporary Harness, coordinator, or recovery-specific result path. A second
// run finds no running Operation and writes nothing.
func recoverRunningOperation(ctx context.Context, store Storage, sessionID, operationID string) error {
	return store.Transact(ctx, func(tx Transaction) error {
		sreg, err := tx.ReadRegister(RegisterKey{SessionID: sessionID, Kind: RegisterSession})
		if err != nil {
			return err
		}
		currentSession, err := decodeSessionRegister(sreg)
		if err != nil {
			return corruptSession(sessionID, "session register: %v", err)
		}
		oreg, err := tx.ReadRegister(RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID})
		if err != nil {
			return err
		}
		currentOp, err := decodeOperationRegister(oreg)
		if err != nil {
			return corruptSession(sessionID, "operation register %q: %v", operationID, err)
		}
		if currentOp.State.Status != OperationRunning { // a second run finds no running Operation and writes nothing
			return nil
		}
		_, _, _, err = commitTerminalSettlement(tx, sessionID, operationID, currentSession, currentOp, sreg.Revision, oreg.Revision, OperationInterruption, recoveryInterruptedDetail, nil, model.ModelRef{}, nil)
		return err
	})
}
