package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/harness"
)

// ErrDenied is the one authority class: the command target lies outside the
// caller's granted scope.
var ErrDenied = errors.New("runtime: denied")

// callerScopeKind is the closed union kind used for both a caller grant and
// one command target.
type callerScopeKind string

const (
	callerScopeHarness   callerScopeKind = "harness"
	callerScopeWorkspace callerScopeKind = "workspace"
	callerScopeSession   callerScopeKind = "session"
)

// callerScope is one concrete grant or command target: the whole owner, one
// Workspace and its Sessions, or one Session.
type callerScope struct {
	kind      callerScopeKind
	workspace string
	sessionID string
}

// harnessScope is the whole-owner target, which is the same value as the
// whole-owner grant.
var harnessScope = callerScope{kind: callerScopeHarness}

// callerContext is the trusted host's grant input.
type callerContext struct {
	Origin      harness.InputOrigin
	ExtensionID string // present exactly for plugin attribution
	Scope       callerScope
}

// caller is one trusted command attribution and target grant. It is issued
// only by the owner; request data never supplies its origin or scope.
type caller struct {
	origin      harness.InputOrigin
	extensionID string
	scope       callerScope
}

// newCaller validates one trusted grant: the origin is the closed
// user/runtime/plugin set with provenance exactly for plugin, and the scope
// is exactly one concrete target union member.
func newCaller(grant callerContext) (caller, error) {
	switch grant.Origin {
	case harness.InputOriginUser, harness.InputOriginRuntime:
		if grant.ExtensionID != "" {
			return caller{}, fmt.Errorf("input origin %q must not carry extension attribution: %w", grant.Origin, harness.ErrInvalid)
		}
	case harness.InputOriginPlugin:
		if grant.ExtensionID == "" {
			return caller{}, fmt.Errorf("plugin caller requires extension attribution: %w", harness.ErrInvalid)
		}
	default:
		return caller{}, fmt.Errorf("caller origin %q is not one of user, runtime or plugin: %w", grant.Origin, harness.ErrInvalid)
	}
	scope := grant.Scope
	switch scope.kind {
	case callerScopeHarness:
		if scope.workspace != "" || scope.sessionID != "" {
			return caller{}, fmt.Errorf("harness scope carries no target identity: %w", harness.ErrInvalid)
		}
	case callerScopeWorkspace:
		if scope.sessionID != "" || scope.workspace == "" || !filepath.IsAbs(scope.workspace) || filepath.Clean(scope.workspace) != scope.workspace {
			return caller{}, fmt.Errorf("workspace scope requires one clean absolute path: %w", harness.ErrInvalid)
		}
	case callerScopeSession:
		if scope.workspace != "" || !canonicalSessionID(scope.sessionID) {
			return caller{}, fmt.Errorf("session scope requires one canonical session id: %w", harness.ErrInvalid)
		}
	default:
		return caller{}, fmt.Errorf("caller scope %q is not one of harness, workspace or session: %w", scope.kind, harness.ErrInvalid)
	}
	return caller{origin: grant.Origin, extensionID: grant.ExtensionID, scope: scope}, nil
}

// protocolCaller is the user attribution the authenticated HTTP host binds
// after authentication: the whole owner and no extension.
func protocolCaller() caller {
	return caller{origin: harness.InputOriginUser, scope: harnessScope}
}

// authorize validates one producer target against the grant: harness scope
// reaches every target, a Workspace grant reaches that Workspace and its
// Sessions, and a Session grant reaches only itself. A Workspace grant
// targeting a Session resolves the Session's immutable Workspace through one
// authoritative read. Everything else is denied before the producer reads or
// transitions.
func (c caller) authorize(ctx context.Context, h *harness.Harness, target callerScope) error {
	switch target.kind {
	case callerScopeHarness:
		if c.scope.kind == callerScopeHarness {
			return nil
		}
	case callerScopeWorkspace:
		switch c.scope.kind {
		case callerScopeHarness:
			return nil
		case callerScopeWorkspace:
			if c.scope.workspace == target.workspace {
				return nil
			}
		}
	case callerScopeSession:
		switch c.scope.kind {
		case callerScopeHarness:
			return nil
		case callerScopeSession:
			if c.scope.sessionID == target.sessionID {
				return nil
			}
		case callerScopeWorkspace:
			if h == nil {
				return fmt.Errorf("session target requires an owner read: %w", ErrDenied)
			}
			header, err := h.ReadSessionHeader(ctx, target.sessionID)
			if err != nil {
				return err
			}
			if header.Identity.Workspace == c.scope.workspace {
				return nil
			}
		}
	}
	return fmt.Errorf("%s target is outside the caller scope: %w", target.kind, ErrDenied)
}

// canonicalSessionID reports whether value is exactly 32 lowercase hex
// digits.
func canonicalSessionID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}
