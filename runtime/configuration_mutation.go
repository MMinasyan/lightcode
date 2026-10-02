package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
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
	// editConnection owns no file: the existing-provider connect and
	// disconnect publish one ready next-generation candidate and event while
	// leaving the main and agents bytes untouched. changed=false never means
	// this kind — a successful repeat connection still consumes a
	// generation.
	editConnection
)

// rawRoots carries the owned decoded root maps of the latest raw input
// files one edit applies to.
type rawRoots struct {
	config map[string]json.RawMessage
	agents map[string]json.RawMessage
}

// connectionKeyAction is the one managed-env key action a connection-bearing
// edit may carry.
type connectionKeyAction int

const (
	keyActionNone connectionKeyAction = iota
	keyActionSet
	keyActionRemove
)

// connectionEffects carries the narrow connection side-effect data of the
// two connection-bearing edit shapes: the existing-provider connect and
// disconnect (owning kind editConnection) and the custom create's optional
// managed-key write after its owning file. It is plain data — the
// connection subject's published identity and transport, the optional
// fetched discovery, and one managed-env key action — not a hook registry:
// the writer itself runs the effects in the one fixed order after the
// complete candidate validates and the final caller-first cancellation
// check passes. The key action targets the service's attached manager; a
// nil manager's action is the manager's own typed failure.
type connectionEffects struct {
	providerID string
	transport  catalog.Transport
	discovered *catalog.DiscoveredProvider
	keyAction  connectionKeyAction
	keyEnv     string
	keyValue   string
}

// configurationEdit is one narrow mutation of the owned decoded root maps of
// the latest raw user layer. apply mutates the roots in place and reports the
// owning file it edited and whether the edit changed anything; changed=false
// is exclusively an explicit no-user-override reset that found nothing to
// edit, and a successful edit is always changed=true, identical bytes
// included. check is the optional pure edited-subject check over the built
// candidate — it runs after the one build and before the write, so a touched
// subject the catalog validation dropped is refused without a write, and no
// fallible projection ever runs after the commit. connection carries the one
// narrow side-effect chain of the connection-bearing edits; nil on every
// other edit.
type configurationEdit struct {
	apply      func(rawRoots) (editedFile, bool, error)
	check      func(*configuration) error
	connection *connectionEffects
}

// mutate applies one edit to the latest raw user layer and publishes the
// complete validated candidate through the shared construction and
// publication path. It holds the build mutex from input read through
// publication, so Lightcode-side edits and reloads serialize. The latest
// owning and companion raw user files are read inside that hold — never a
// stale published snapshot — and decoded as root objects whose raw members
// keep their exact bytes; a missing file contributes its skeleton bytes in
// memory and is created by no read. The edit's owning file — and only that
// file — is atomically rewritten mode 0600 after the candidate validates,
// the edit's candidate check passes, and the final caller-first cancellation
// check passes; a connection-bearing edit owns no file and runs its
// side-effect chain in the same position. A no-edit result returns the
// current publication without consuming a generation; a failed candidate, a
// failed check, or a cancellation before the write leaves the file and
// publication unchanged; after the write starts, no further cancellation
// check, rebuild, or fallible stage runs before the ready snapshot, its one
// event, and the mutex release — an admitted call completes publication
// despite caller disconnect or owner shutdown. A connection-bearing create's
// managed-key failure restores the exact prior owning bytes before returning
// and joins that restore error with the key failure; no atomic success is
// claimed.
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
	configData, configExisted, err := readCapturedBytes(s.configPath, mainConfigSkeleton)
	if err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(fmt.Errorf("read main configuration: %w", err))
	}
	agentsData, _, err := readCapturedBytes(s.agentsPath, agentsSkeleton)
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
	owning, changed, err := edit.apply(rawRoots{config: configRoot, agents: agentsRoot})
	if err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	if !changed {
		// The explicit no-user-override reset: no edit, no write, no new
		// generation. A connection edit never takes this path.
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
	case editConnection:
		// No owning file: the candidate publishes over the untouched bytes.
	}
	// The connection's overlay decision is made under the build mutex
	// against the live published identity: a concurrent valid writer that
	// already supplied usable models under the same identity skips both the
	// in-memory overlay and the cache write. Only a connection-bearing edit
	// — one owning no file — selects the non-refreshing connection loader;
	// every other edit builds over the ordinary LoadCaptured inputs.
	var conn *connectionEffects
	if owning == editConnection {
		conn = edit.connection
	}
	if conn != nil && conn.discovered != nil {
		if live := s.current().catalog.Providers[conn.providerID]; live != nil && usableModelCount(live) > 0 {
			conn.discovered = nil
		}
	}
	candidate, err := s.buildCaptured(ctx, generation, configCandidate, agentsCandidate, conn)
	if err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	// The edit's pure edited-subject check runs once over the built
	// candidate, before the write: a touched subject the shared catalog
	// validation dropped — or a connection subject whose live identity
	// changed or was removed during the gated fetch — refuses the edit here,
	// with the file and the publication untouched. Nothing fallible runs
	// after the write.
	if edit.check != nil {
		if err := edit.check(candidate); err != nil {
			s.buildMu.Unlock()
			return nil, configurationFailure(err)
		}
	}
	// The final caller-first cancellation check, immediately before the
	// first side effect. After it, nothing fallible precedes the ready
	// publication except the two connection effects, whose failures are
	// typed and publish nothing.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return nil, err
	}
	if owning == editConnection {
		if err := s.applyConnectionEffects(edit.connection); err != nil {
			s.buildMu.Unlock()
			return nil, err
		}
		s.commit(candidate)
		return candidate, nil
	}
	if err := atomicfs.Write(owningPath, complete, 0o600); err != nil {
		s.buildMu.Unlock()
		return nil, configurationFailure(fmt.Errorf("write %s: %w", owningPath, err))
	}
	if conn := edit.connection; conn != nil && conn.keyAction == keyActionSet {
		if err := setManagedKey(s.env, conn.keyEnv, conn.keyValue); err != nil {
			// The restore runs despite a canceled caller: the exact prior
			// owning bytes go back (or the prior file absence returns) and
			// the joined failure claims no atomic success.
			restoreErr := restoreOwningBytes(owningPath, configData, configExisted)
			s.buildMu.Unlock()
			return nil, configurationFailure(errors.Join(fmt.Errorf("persist API key %s: %w", conn.keyEnv, err), restoreErr))
		}
	}
	s.commit(candidate)
	return candidate, nil
}

// applyConnectionEffects runs an existing-provider connection's external
// side effects in the one fixed order: the accepted discovery cache write,
// then the one managed-env key action. The caller holds the build mutex and
// the candidate is already validated; every failure is a typed
// configuration failure with nothing published. The discovery cache and the
// managed env are not one rollback transaction — a cache write that
// succeeded before a later key failure is honest partial state.
func (s *configurationService) applyConnectionEffects(conn *connectionEffects) error {
	if conn.discovered != nil {
		ok, err := catalog.TryWriteDiscoveryCache(s.loader.Home(), conn.providerID, conn.transport, *conn.discovered, time.Now().UTC())
		if err != nil {
			return configurationFailure(fmt.Errorf("write discovery cache for provider %q: %w", conn.providerID, err))
		}
		if !ok {
			return configurationFailure(fmt.Errorf("discovery lock is held for %q; retry", conn.providerID))
		}
	}
	switch conn.keyAction {
	case keyActionSet:
		if err := setManagedKey(s.env, conn.keyEnv, conn.keyValue); err != nil {
			return configurationFailure(fmt.Errorf("persist API key %s: %w", conn.keyEnv, err))
		}
	case keyActionRemove:
		// The nil-manager case never plans a removal: the disconnect
		// resolves actual ownership before this stage.
		if err := s.env.TryRemove(conn.keyEnv); err != nil {
			return configurationFailure(fmt.Errorf("remove managed key %s: %w", conn.keyEnv, err))
		}
	}
	return nil
}

// setManagedKey persists one credential through the supplied manager with
// the retained ownership resolution: an ErrExternalKey refusal is tolerated
// only when the now-external value is nonempty — the shell key wins and is
// never overwritten — and every other failure, a nil manager's typed
// refusal included, returns unchanged.
func setManagedKey(env *config.ManagedEnv, name, value string) error {
	err := env.TrySet(name, value)
	if errors.Is(err, config.ErrExternalKey) && os.Getenv(name) != "" {
		return nil
	}
	return err
}

// restoreOwningBytes puts the exact prior owning bytes back after a failed
// key write: the atomic write replays them, a previously absent file is
// removed again. The caller holds the build mutex.
func restoreOwningBytes(path string, prior []byte, existed bool) error {
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restore absent %s: %w", path, err)
		}
		return nil
	}
	if err := atomicfs.Write(path, prior, 0o600); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	return nil
}

// editSettings replaces the complete settings shape wholesale: the whole
// sessions member and the whole plugins member are written as the generated
// target shape — an omitted optional plugin section removes any newer
// section, and a section present only in the old raw document is absent
// from the new complete plugins document. Never merged per field. Every
// other top-level root member is unowned and kept untouched.
func editSettings(settings protocol.Settings) configurationEdit {
	return configurationEdit{apply: func(roots rawRoots) (editedFile, bool, error) {
		sessions, err := json.Marshal(settings.Sessions)
		if err != nil {
			return 0, false, err
		}
		plugins, err := json.Marshal(settings.Plugins)
		if err != nil {
			return 0, false, err
		}
		roots.config["sessions"] = sessions
		roots.config["plugins"] = plugins
		return editMainConfig, true, nil
	}}
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
	return configurationEdit{apply: func(roots rawRoots) (editedFile, bool, error) {
		agentsRoot := roots.agents
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
	}}
}

// readCapturedBytes reads one input file's latest bytes without side
// effects: a missing file contributes the skeleton bytes in memory and is
// created only by an owning write or the retained skeleton-creating reload
// capture. The boolean reports the file's prior existence — a create's
// failed key write restores it by removing the file again.
func readCapturedBytes(path, skeleton string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []byte(skeleton), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
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
