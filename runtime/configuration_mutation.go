package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file owns the one validated configuration writer: mutations apply a
// private edit to the latest raw user layer and publish the complete
// candidate through the same construction and publication path as Reload.

// editedFile names the owning input file one edit writes: the only file the
// mutation touches, atomically, mode 0600, after the complete candidate
// validates.
type editedFile int

const (
	editMainConfig editedFile = iota + 1
	editAgentsFile
)

// configurationEdit mutates the owned decoded root maps of the latest raw
// input files in place and reports the owning file it edited and whether the
// edit changed anything. changed=false is exclusively an explicit
// no-user-override reset that found nothing to edit; a successful edit is
// always changed=true, identical bytes included.
type configurationEdit func(configRoot, agentsRoot map[string]json.RawMessage) (editedFile, bool, error)

// mutate applies one edit to the latest raw user layer and publishes the
// complete validated candidate through the shared construction and
// publication path. It holds the build mutex from input read through
// publication, so Lightcode-side edits and reloads serialize. The latest
// owning and companion raw user files are read inside that hold — never a
// stale published snapshot — and decoded as root objects whose raw members
// keep their exact bytes; a missing file contributes its skeleton bytes in
// memory and is created by no read. The edit's owning file — and only that
// file — is atomically rewritten mode 0600 after the candidate validates and
// the final caller-first cancellation check passes. A no-edit result returns
// the current publication without consuming a generation; a failed candidate
// or a cancellation before the write leaves the file and publication
// unchanged; after the write starts, no further cancellation check, rebuild,
// or fallible stage runs before the ready snapshot, its one event, and the
// mutex release — an admitted call completes publication despite caller
// disconnect or owner shutdown.
func (s *configurationService) mutate(ctx context.Context, edit configurationEdit) (*configuration, error) {
	if err := s.canceled(ctx); err != nil {
		return nil, err
	}
	s.buildMu.Lock()
	// A cancellation observed while waiting is returned after the active
	// builder releases the mutex, without starting another build.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	configData, err := readCapturedBytes(s.configPath, mainConfigSkeleton)
	if err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(fmt.Errorf("read main configuration: %w", err))
	}
	agentsData, err := readCapturedBytes(s.agentsPath, agentsSkeleton)
	if err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(fmt.Errorf("read agent definitions: %w", err))
	}
	configRoot, err := decodeRoot(configData)
	if err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(err)
	}
	agentsRoot, err := decodeRoot(agentsData)
	if err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(err)
	}
	owning, changed, err := edit(configRoot, agentsRoot)
	if err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	if !changed {
		// The explicit no-user-override reset: no edit, no write, no new
		// generation.
		s.buildMu.Unlock()
		return s.published.Load(), nil
	}
	generation, err := s.nextGeneration()
	if err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	configCandidate, agentsCandidate := configData, agentsData
	var owningPath string
	var complete []byte
	switch owning {
	case editMainConfig:
		owningPath = s.configPath
		if complete, err = marshalRoot(configRoot); err != nil {
			s.buildMu.Unlock()
			return nil, configurationFailure(err)
		}
		configCandidate = complete
	case editAgentsFile:
		owningPath = s.agentsPath
		if complete, err = marshalRoot(agentsRoot); err != nil {
			s.buildMu.Unlock()
			return nil, configurationFailure(err)
		}
		agentsCandidate = complete
	}
	candidate, err := s.buildCaptured(ctx, generation, configCandidate, agentsCandidate)
	if err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	// The final caller-first cancellation check, immediately before the
	// atomic write of the owning file. After the write starts nothing
	// fallible precedes the ready publication.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	if err := atomicfs.Write(owningPath, complete, 0o600); err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(fmt.Errorf("write %s: %w", owningPath, err))
	}
	s.commit(candidate)
	return candidate, nil
}

// editSettings replaces the complete settings shape wholesale: the whole
// sessions member and the whole plugins member are written as the generated
// target shape — an omitted optional plugin section removes any newer
// section, and a section present only in the old raw document is absent
// from the new complete plugins document. Never merged per field. Every
// other top-level root member is unowned and kept untouched.
func editSettings(settings protocol.Settings) configurationEdit {
	return func(configRoot, _ map[string]json.RawMessage) (editedFile, bool, error) {
		sessions, err := json.Marshal(settings.Sessions)
		if err != nil {
			return 0, false, err
		}
		plugins, err := json.Marshal(settings.Plugins)
		if err != nil {
			return 0, false, err
		}
		configRoot["sessions"] = sessions
		configRoot["plugins"] = plugins
		return editMainConfig, true, nil
	}
}

// editAgentModel copies the retained owning model-field mutation onto the
// latest agents root: a nonempty ref replaces the user model override, an
// empty ref clears it. Every successful set or clear — absent override and
// identical bytes included — is a real edit that writes the owning file and
// publishes the next generation; only the retained public field reset
// operator carries a no-override exception, and this edit is not it. The
// known-type check resolves the latest raw
// bytes' definitions through the ordinary admission projection, so unknown
// or dropped/invalid types fail harness.ErrInvalid before any bare
// definition is created; builtins that need no explicit user entry stay
// addressable.
func (s *configurationService) editAgentModel(agentType, ref string) configurationEdit {
	return func(_, agentsRoot map[string]json.RawMessage) (editedFile, bool, error) {
		if ref != "" {
			if _, err := model.Parse(ref); err != nil {
				return 0, false, fmt.Errorf("model ref %q: %v: %w", ref, err, harness.ErrInvalid)
			}
		}
		data, err := json.Marshal(agentsRoot)
		if err != nil {
			return 0, false, err
		}
		parsed, err := agents.ParseWithCapabilities(data, s.capabilityIDs, s.toolIDs, s.defaultCapabilityIDs)
		if err != nil {
			return 0, false, configurationFailure(err)
		}
		if _, err := harness.ResolveAgentType(agentType, projectAgentTypes(parsed.All())); err != nil {
			return 0, false, err
		}
		var fields map[string]json.RawMessage
		if raw, ok := agentsRoot[agentType]; ok {
			if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
				return 0, false, fmt.Errorf("agents.%s must be an object: %w", agentType, harness.ErrInvalid)
			}
		} else {
			fields = map[string]json.RawMessage{}
		}
		if ref == "" {
			delete(fields, "model")
		} else {
			if fields["model"], err = json.Marshal(ref); err != nil {
				return 0, false, err
			}
		}
		if agentsRoot[agentType], err = json.Marshal(fields); err != nil {
			return 0, false, err
		}
		return editAgentsFile, true, nil
	}
}

// readCapturedBytes reads one input file's latest bytes without side
// effects: a missing file contributes the skeleton bytes in memory and is
// created only by an owning write or the retained skeleton-creating reload
// capture.
func readCapturedBytes(path, skeleton string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []byte(skeleton), nil
	}
	return data, err
}

// decodeRoot decodes one input document into its root object with every
// member's raw bytes preserved: numbers keep their exact lexemes, a null
// document is the empty root, and a non-object document or trailing content
// is rejected.
func decodeRoot(data []byte) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("decode root object: %w", err)
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}
	return root, nil
}

// marshalRoot serializes one edited root map as the owning file's complete
// bytes: sorted members, unowned JSON values preserved verbatim so numeric
// lexemes survive, one trailing newline like the retained writers. Strings
// may be HTML-escaped by the encoder; values and number tokens are retained.
func marshalRoot(root map[string]json.RawMessage) ([]byte, error) {
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode edited configuration: %w", err)
	}
	return append(data, '\n'), nil
}
