package tool

import (
	"fmt"
	"io"
	"os"

	"github.com/MMinasyan/lightcode/internal/safefs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// SnapshotStore is the complete internal snapshot surface the prepared write,
// edit and patch mutations require. It is mandatory: mutation never runs
// against a missing or partial store.
type SnapshotStore interface {
	CurrentTurn() int
	SnapshotResolvedEntry(turn int, originalPath, canonicalPath string) (entryID string, created bool, err error)
	DiscardSnapshotEntry(turn int, entryID string) error
	RecordSnapshotContent(turn int, entryID string, content []byte) error
	RecordSnapshotAbsence(turn int, entryID string) error
	ReplaceSnapshotIdentity(turn int, entryID string, identity *snapshot.SnapshotContentIdentity) (*snapshot.SnapshotContentIdentity, error)
}

type mutationFile interface {
	io.Reader
	io.Seeker
	io.Writer
	Close() error
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(size int64) error
}

var (
	openWriteTargetForMutationFunc = openWriteTargetForMutationReal
	openExistingMutationFile       = func(path string, flag int) (mutationFile, error) {
		return safefs.OpenExisting(path, flag)
	}
)

// snapshotEntry is one captured mutation snapshot: the entry id, whether this
// call created the preimage, and the LastWrite proof the entry owned before
// the pre-mutation clear.
type snapshotEntry struct {
	store   SnapshotStore
	turn    int
	entryID string
	created bool
	prior   *snapshot.SnapshotContentIdentity
}

// captureSnapshotEntry captures the preimage and persists the cleared
// LastWrite proof under the existing execution/artifact ownership. A failed
// clear abandons the capture (a newly created preimage is discarded) and the
// caller must not mutate.
func captureSnapshotEntry(store SnapshotStore, originalPath, canonicalPath string) (snapshotEntry, error) {
	turn := store.CurrentTurn()
	entryID, created, err := store.SnapshotResolvedEntry(turn, originalPath, canonicalPath)
	if err != nil {
		return snapshotEntry{}, err
	}
	prior, err := store.ReplaceSnapshotIdentity(turn, entryID, nil)
	if err != nil {
		if created {
			if discardErr := store.DiscardSnapshotEntry(turn, entryID); discardErr != nil {
				return snapshotEntry{}, fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
			}
		}
		return snapshotEntry{}, err
	}
	return snapshotEntry{store: store, turn: turn, entryID: entryID, created: created, prior: prior}, nil
}

// abandonUnmutatedSnapshot resolves a proven non-mutation failure: this call's
// newly created preimage is discarded, and an existing entry gets its owned
// prior proof restored. A failed restoration leaves the cleared nil proof,
// reports its error, and preserves the preimage for a later restore skip.
func abandonUnmutatedSnapshot(entry snapshotEntry) error {
	if entry.created {
		return entry.store.DiscardSnapshotEntry(entry.turn, entry.entryID)
	}
	if entry.prior == nil {
		return nil
	}
	if _, err := entry.store.ReplaceSnapshotIdentity(entry.turn, entry.entryID, entry.prior); err != nil {
		return fmt.Errorf("restore snapshot proof: %w", err)
	}
	return nil
}

// writeReplacement performs the truncate/seek/write/sync content replacement
// and reports, independently of the returned error, whether the intended
// bytes are known to have been fully produced: only a byte count that covers
// the whole content claims a complete identity. A truncate, seek, or short
// write leaves the produced state uncertain.
func writeReplacement(f mutationFile, content []byte) (complete bool, err error) {
	if err := f.Truncate(0); err != nil {
		return false, fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek: %w", err)
	}
	n, writeErr := f.Write(content)
	if n != len(content) {
		if writeErr != nil {
			return false, fmt.Errorf("write: incomplete %d of %d bytes: %w", n, len(content), writeErr)
		}
		return false, fmt.Errorf("write: incomplete %d of %d bytes", n, len(content))
	}
	if writeErr != nil {
		return true, fmt.Errorf("write: %w", writeErr)
	}
	if err := f.Sync(); err != nil {
		return true, fmt.Errorf("sync: %w", err)
	}
	return true, nil
}

// finishMutationFailure resolves a mutation error against the snapshot entry:
// a proven non-mutation abandons the entry; a mutation whose complete
// produced content is known records that producer-owned identity; an
// uncertain started mutation retains the preimage with the cleared nil
// identity. No post-error filesystem reread establishes output.
func finishMutationFailure(entry snapshotEntry, started, complete bool, produced []byte, err error) error {
	if !started {
		if abandonErr := abandonUnmutatedSnapshot(entry); abandonErr != nil {
			return fmt.Errorf("%w; additionally failed to abandon snapshot: %v", err, abandonErr)
		}
		return err
	}
	if complete {
		if recordErr := entry.store.RecordSnapshotContent(entry.turn, entry.entryID, produced); recordErr != nil {
			return fmt.Errorf("%w; additionally failed to record snapshot identity: %v", err, recordErr)
		}
	}
	return err
}

// recordProducedSnapshotContent records the identity of content the producer
// completed. A failure leaves the cleared nil proof and reports its error;
// the retained preimage serves a later restore skip.
func recordProducedSnapshotContent(entry snapshotEntry, content []byte) error {
	return entry.store.RecordSnapshotContent(entry.turn, entry.entryID, content)
}

func recordProducedSnapshotAbsence(entry snapshotEntry) error {
	return entry.store.RecordSnapshotAbsence(entry.turn, entry.entryID)
}

// openWriteTargetForMutation opens the canonical write target through the
// shared safefs primitives.

func openWriteTargetForMutation(absPath string) (mutationFile, bool, error) {
	return openWriteTargetForMutationFunc(absPath)
}

func openWriteTargetForMutationReal(absPath string) (mutationFile, bool, error) {
	_, err := ensureRegularExistingTarget(absPath)
	if err != nil {
		return nil, false, err
	}

	f, openedExisting, err := safefs.OpenForWrite(absPath, 0o644)
	if err != nil {
		return nil, false, err
	}
	if openedExisting {
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, false, fmt.Errorf("stat: %w", err)
		}
		if err := ensureRegularFileInfo(absPath, info); err != nil {
			_ = f.Close()
			return nil, false, err
		}
	}
	return f, openedExisting, nil
}

func preflightWriteSnapshotTarget(absPath string) error {
	existed, err := ensureRegularExistingTarget(absPath)
	if err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if !existed {
		return nil
	}
	f, err := safefs.OpenExisting(absPath, os.O_RDWR)
	if err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("write_file: stat: %w", err)
	}
	if err := ensureRegularFileInfo(absPath, info); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	return nil
}
