package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
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
	// editConnection owns no file: the existing-provider connect and
	// disconnect publish one ready next-generation candidate and event while
	// leaving the main document untouched. Every successful
	// connection edit therefore consumes a generation.
	editConnection
)

// rawRoots carries the owned decoded root map one edit applies to.
type rawRoots struct {
	config map[string]json.RawMessage
}

// connectionKeyAction is the one credential action a connection-bearing
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
// fetched discovery, and one credential action — not a hook registry:
// the writer itself runs the effects in the one fixed order after the
// complete candidate validates and the final Caller-first cancellation
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
// owning file it edited; it receives the mutation's one short capture of the
// ready revision, so every subject and credential decision consumes the same
// owned observation. Every successful apply — an absent-member reset that
// changed no member included — is a real edit that writes its owning file and
// publishes the next generation. check is the optional pure edited-subject
// check over the built candidate and the same capture — it runs after the one
// build and before the write, so a touched subject the catalog validation
// dropped is refused without a write, and no fallible projection ever runs
// after the commit. connection carries the one narrow side-effect chain of
// the connection-bearing edits; nil on every other edit.
type configurationEdit struct {
	apply      func(rawRoots, configurationCapture) (editedFile, error)
	check      func(*configuration, configurationCapture) error
	connection *connectionEffects
}

// mutate applies one edit to the latest raw user layer and publishes the
// complete validated candidate through the shared construction and
// publication path. It holds the build mutex from input read through
// publication, so Lightcode-side edits and reloads serialize. One capture of
// the ready revision is taken without a reader-visible lock and released
// again before the candidate build, so the edit's subject and credential
// decisions never compose live getters and a slow build never blocks readers.
// The latest main document is read from the configured source inside that
// hold — never a stale published snapshot — and decoded as a root object whose
// raw members keep their exact bytes; the source never creates a missing
// document. The edit's owning document — and only that document — is
// atomically replaced after the candidate validates, the edit's candidate
// check passes, and the final Caller-first cancellation check passes; a
// connection-bearing edit owns no document and runs its side-effect chain in
// the same position. Every successful apply writes and publishes the next
// generation, an absent-member reset that changed nothing included; a failed
// candidate, a failed check, or a cancellation before the write leaves the
// source and publication unchanged; after the write starts, no further
// cancellation check, rebuild, or fallible capture runs before the ready
// snapshot, its one event, and the build-mutex release — an admitted call
// completes publication despite Caller disconnect or owner shutdown. A
// connection-bearing create's credential failure restores the exact prior
// owning bytes before returning and joins that restore error with the key
// failure; no atomic success is claimed. Every error path releases each
// acquired ownership exactly once.
func (s *configurationService) mutate(ctx context.Context, edit configurationEdit) (configurationCapture, error) {
	if err := s.canceled(ctx); err != nil {
		return configurationCapture{}, err
	}
	s.buildMu.Lock()
	// A cancellation observed while waiting is returned after the active
	// builder releases the mutex, without starting another build.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	configData, err := s.configuration.Read(ctx)
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, configurationFailure(fmt.Errorf("read main configuration: %w", err))
	}
	configRoot, err := decodeRoot(configData)
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, configurationFailure(err)
	}
	// The build's own capture over the latest document's names. Edit
	// decisions read this observation, never the previous publication; the
	// candidate build takes its own single capture over the candidate's full
	// name union after the edit applies.
	seed, err := s.mutationCapture(ctx, configData)
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	owning, err := edit.apply(rawRoots{config: configRoot}, seed)
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	generation, err := s.nextGeneration()
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	configCandidate := configData
	var complete []byte
	switch owning {
	case editMainConfig:
		if complete, err = marshalRoot(configRoot); err != nil {
			s.buildMu.Unlock()
			return configurationCapture{}, configurationFailure(err)
		}
		configCandidate = complete
	case editConnection:
		// No owning file: the candidate publishes over the untouched bytes.
	}
	// The connection's overlay decision is made under the build mutex
	// against the captured published identity: a concurrent valid writer that
	// already supplied usable models under the same identity skips both the
	// in-memory overlay and the cache write. Only a connection-bearing edit
	// — one owning no file — selects the non-refreshing connection loader;
	// every other edit builds over the ordinary LoadCaptured inputs.
	var conn *connectionEffects
	if owning == editConnection {
		conn = edit.connection
	}
	if conn != nil && conn.discovered != nil {
		if live := seed.snapshot.catalog.Providers[conn.providerID]; live != nil && usableModelCount(live) > 0 {
			conn.discovered = nil
		}
	}
	candidate, err := s.buildCaptured(ctx, generation, configCandidate, conn, edit.connection)
	if err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	fresh := s.freshResources(candidate)
	defer func() {
		if fresh != nil {
			_ = fresh.release()
		}
	}()
	// The edit's pure edited-subject check runs once over the built
	// candidate and the mutation's capture, before the write: a touched
	// subject the shared catalog validation dropped — or a connection
	// subject whose live identity changed or was removed during the gated
	// fetch — refuses the edit here, with the file and the publication
	// untouched. Nothing fallible runs after the write.
	if edit.check != nil {
		if err := edit.check(candidate, seed); err != nil {
			s.buildMu.Unlock()
			return configurationCapture{}, configurationFailure(err)
		}
	}
	// The final Caller-first cancellation check, immediately before the
	// first side effect. After it, nothing fallible precedes the ready
	// publication except the two connection effects, whose failures are
	// typed and publish nothing.
	if err := s.canceled(ctx); err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, err
	}
	// Cross-source writes and their rollback are not abandoned by request
	// cancellation once the final check has passed.
	writeCtx := context.WithoutCancel(ctx)
	if owning == editConnection {
		// A connection-bearing edit owns no file: its candidate capture is
		// already published with the build, and its mutable credential effect
		// runs before the ready publication.
		if err := s.applyConnectionEffects(writeCtx, edit.connection); err != nil {
			s.buildMu.Unlock()
			return configurationCapture{}, err
		}
		fresh = nil
		return s.commit(candidate), nil
	}
	if err := s.configuration.Write(writeCtx, complete); err != nil {
		s.buildMu.Unlock()
		return configurationCapture{}, configurationFailure(fmt.Errorf("write main configuration: %w", err))
	}
	if conn := edit.connection; conn != nil && conn.keyAction == keyActionSet {
		// The custom create writes its owning document first, then performs
		// its managed-key effect and rolls the document bytes back on failure.
		// The owner's actual refusal — a concurrent external change detected
		// at the write included — propagates; no ErrExternalKey tolerance
		// disguises it as success.
		if err := s.credentials.Set(writeCtx, conn.keyEnv, conn.keyValue); err != nil {
			// The restore runs despite a canceled Caller: the exact prior
			// owning bytes go back and the joined failure claims no atomic
			// success.
			restoreErr := s.configuration.Write(writeCtx, configData)
			s.buildMu.Unlock()
			return configurationCapture{}, configurationFailure(errors.Join(fmt.Errorf("persist API key %s: %w", conn.keyEnv, err), restoreErr))
		}
		fresh = nil
		return s.commit(candidate), nil
	}
	fresh = nil
	return s.commit(candidate), nil
}

// mutationCapture builds the mutation build's credential observation from the
// latest document's provider and active declared names before the edit runs;
// it feeds only the edit's decisions. The returned capture carries that
// observation with the latest assembled catalog while retaining the published
// execution resources for extension callbacks; the candidate build takes its
// own single capture over the edited document's names.
func (s *configurationService) mutationCapture(ctx context.Context, configData []byte) (configurationCapture, error) {
	doc, err := decodeCapturedConfig(configData)
	if err != nil {
		return configurationCapture{}, configurationFailure(err)
	}
	var active []extension.Native
	for _, native := range s.natives {
		if sectionEnabled(doc.Extensions[native.Declaration.ID]) {
			active = append(active, native)
		}
	}
	built, err := s.loader.AssembleCaptured(ctx, doc.Providers)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return configurationCapture{}, err
		}
		return configurationCapture{}, configurationFailure(err)
	}
	secrets := resolveDeclaredSecrets(active, doc.Extensions)
	credentials, err := s.captureCredentials(ctx, capturedCredentialNames(built.Catalog, secrets), nil)
	if err != nil {
		return configurationCapture{}, configurationFailure(err)
	}
	var snapshot configuration
	if published := s.current(); published != nil {
		snapshot = *published
	}
	snapshot.catalog = built.Catalog
	snapshot.catalogWarnings = built.Warnings
	snapshot.secrets = secrets
	snapshot.credentials = credentials
	snapshot.declaredBindings = captureDeclaredBindings(secrets, credentials)
	return configurationCapture{snapshot: &snapshot}, nil
}

// applyConnectionEffects runs an existing-provider connection's external
// side effects in the one fixed order: the accepted discovery cache write
// when a cache location is selected, then the one owned credential action.
// The Caller holds the build mutex and the candidate is already validated;
// every failure is a typed configuration failure with nothing published. The
// discovery cache and credentials are not one rollback transaction — a cache
// write that succeeded before a later key failure is honest partial state. An
// empty discovery cache location skips the cache write without error.
func (s *configurationService) applyConnectionEffects(ctx context.Context, conn *connectionEffects) error {
	cacheUpdated := false
	if conn.discovered != nil && s.locations.DiscoveryCacheDir != "" {
		ok, err := catalog.TryWriteDiscoveryCache(s.locations.DiscoveryCacheDir, conn.providerID, conn.transport, *conn.discovered, time.Now().UTC())
		if err != nil {
			return configurationFailure(fmt.Errorf("write discovery cache for provider %q: %w", conn.providerID, err))
		}
		if !ok {
			return configurationFailure(fmt.Errorf("discovery lock is held for %q; retry", conn.providerID))
		}
		cacheUpdated = true
	}
	failure := func(err error) error {
		if cacheUpdated {
			return configurationFailure(fmt.Errorf("%w; the discovery cache for provider %q was updated", err, conn.providerID))
		}
		return configurationFailure(err)
	}
	switch conn.keyAction {
	case keyActionSet:
		// The owner's actual refusal propagates: an external variable
		// detected at the write or a concurrent change under the leaf lock —
		// no tolerance disguises either as success.
		if err := s.credentials.Set(ctx, conn.keyEnv, conn.keyValue); err != nil {
			return failure(fmt.Errorf("persist API key %s: %w", conn.keyEnv, err))
		}
	case keyActionRemove:
		if err := s.credentials.Remove(ctx, conn.keyEnv); err != nil {
			return failure(fmt.Errorf("remove managed key %s: %w", conn.keyEnv, err))
		}
	}
	return nil
}

// editSettings builds the settings edit: the whole sessions member, the
// top-level baseline tools, jobs and compaction sections, and the whole
// extensions member are written as the generated target shape — each baseline
// section as its exact JSON object bytes and each extension ID's opaque document
// string becomes its raw section, and an ID absent from the
// target removes any newer section, exactly one object per document — and
// every other top-level root member is unowned and kept untouched. Never
// merged per field. The selected declaration's validator, not this edit,
// interprets each active document; candidate construction validates the
// baseline sections with their owning services' validators.
func editSettings(settings protocol.Settings) configurationEdit {
	return configurationEdit{apply: func(roots rawRoots, _ configurationCapture) (editedFile, error) {
		sessions, err := json.Marshal(settings.Sessions)
		if err != nil {
			return 0, err
		}
		plugins := make(map[string]json.RawMessage, len(settings.Extensions))
		for id, document := range settings.Extensions {
			plugins[id] = json.RawMessage(document) // string→[]byte conversion owns the bytes
		}
		pluginsRaw, err := marshalRawObject(plugins, true)
		if err != nil {
			return 0, configurationFailure(fmt.Errorf("encode plugins documents: %w", err))
		}
		roots.config["sessions"] = sessions
		roots.config["extensions"] = pluginsRaw
		roots.config["tools"] = settingsSection(settings.Tools)
		roots.config["jobs"] = settingsSection(settings.Jobs)
		roots.config["compaction"] = settingsSection(settings.Compaction)
		return editMainConfig, nil
	}}
}

// settingsSection copies one top-level baseline settings object; an absent
// member is the empty object the owning service defaults from, matching the
// projection's absent-section value.
func settingsSection(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return append(json.RawMessage(nil), raw...)
}

// editExtension invokes one installed active extension's declared pure
// configuration edit over the latest raw owning section and replaces only
// that extension's member. The top-level call retains one short resource
// reservation; the provider call reuses the prepared Harness instance through
// the one guarded provider path. The writer's build serialization stays held,
// no publication lock is held during the callback, and every other
// extension document keeps its exact bytes.
func (s *configurationService) editExtension(ctx context.Context, extensionID, editID string, arguments json.RawMessage) configurationEdit {
	return configurationEdit{apply: func(roots rawRoots, seed configurationCapture) (editedFile, error) {
		if len(roots.config["extensions"]) == 0 {
			return 0, fmt.Errorf("extension %q is unavailable: %w", extensionID, harness.ErrInvalid)
		}
		extensions, err := decodeRoot(roots.config["extensions"])
		if err != nil {
			return 0, configurationFailure(err)
		}
		if !s.nativeIDs[extensionID] || !sectionEnabled(extensions[extensionID]) {
			return 0, fmt.Errorf("extension %q is unavailable: %w", extensionID, harness.ErrInvalid)
		}
		if seed.snapshot == nil || seed.snapshot.composition == nil || seed.snapshot.resources == nil || seed.snapshot.resources.runtime == nil {
			return 0, fmt.Errorf("extension %q is unavailable: %w", extensionID, harness.ErrInvalid)
		}
		declaration, ok := seed.snapshot.composition.compiled.Declaration(extensionID)
		if !ok {
			return 0, fmt.Errorf("extension %q is unavailable: %w", extensionID, harness.ErrInvalid)
		}
		var edit *extension.ConfigurationEdit
		for i := range declaration.Declaration.Edits {
			if declaration.Declaration.Edits[i].ID == editID {
				edit = &declaration.Declaration.Edits[i]
				break
			}
		}
		if edit == nil {
			return 0, fmt.Errorf("extension %q does not declare configuration edit %q: %w", extensionID, editID, harness.ErrInvalid)
		}
		capability, ok := declaration.Capabilities[edit.Method.CapabilityID]
		if !ok {
			return 0, fmt.Errorf("extension %q edit %q: capability %q is unavailable: %w", extensionID, editID, edit.Method.CapabilityID, harness.ErrInvalid)
		}
		value, ok := seed.snapshot.resources.runtime.capabilities[edit.Method.CapabilityID]
		if !ok {
			return 0, fmt.Errorf("extension %q edit %q: provider is unavailable: %w", extensionID, editID, ErrClosed)
		}
		seed.snapshot.resources.retain()
		defer func() { _ = seed.snapshot.resources.release() }()
		identity := callIdentity{revision: seed.snapshot.resources.revisionString(), grant: nativeGrant{pure: true}}
		result, err := seed.snapshot.composition.invokeProvider(ctx, value, capability.Methods, edit.Method.Method, arguments, extensions[extensionID], identity, false)
		if err != nil {
			// The host-initiated call's argument document is Caller input;
			// its compiled-schema refusal is an invalid request, not an
			// internal composition failure.
			if errors.Is(err, ErrComposition) {
				return 0, fmt.Errorf("%w: %w", err, harness.ErrInvalid)
			}
			return 0, err
		}
		if result.Failure != nil {
			return 0, fmt.Errorf("%s: %s", result.Failure.Code, result.Failure.Message)
		}
		extensions[extensionID] = append(json.RawMessage(nil), result.Value...)
		roots.config["extensions"], err = marshalRawObject(extensions, true)
		if err != nil {
			return 0, err
		}
		return editMainConfig, nil
	}}
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
// bytes: sorted members, extension sections retained verbatim, and other JSON
// values indented without changing numeric lexemes. A trailing newline matches
// the retained writers.
func marshalRoot(root map[string]json.RawMessage) ([]byte, error) {
	return marshalRawObject(root, false)
}

func marshalRawObject(root map[string]json.RawMessage, preserveAll bool) ([]byte, error) {
	keys := make([]string, 0, len(root))
	for key := range root {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		value := root[key]
		if !json.Valid(value) {
			return nil, fmt.Errorf("encode edited configuration: invalid JSON for %q", key)
		}
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		out.WriteByte('\n')
		out.WriteString("  ")
		out.Write(name)
		out.WriteString(": ")
		if preserveAll || key == "extensions" {
			out.Write(value)
		} else {
			var indented bytes.Buffer
			if err := json.Indent(&indented, value, "  ", "  "); err != nil {
				return nil, fmt.Errorf("encode edited configuration: %w", err)
			}
			out.Write(bytes.TrimSpace(indented.Bytes()))
		}
	}
	if len(keys) > 0 {
		out.WriteByte('\n')
	}
	out.WriteString("}\n")
	return out.Bytes(), nil
}
