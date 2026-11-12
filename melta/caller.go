package melta

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MMinasyan/lightcode/harness"
)

// ErrDenied is the one authority class: the command target lies outside the
// Caller's granted scope.
var ErrDenied = errors.New("melta: denied")

// CallerScopeKind is the closed union kind used for both a Caller grant and one
// command target.
type CallerScopeKind string

const (
	CallerScopeHarness   CallerScopeKind = "harness"
	CallerScopeWorkspace CallerScopeKind = "workspace"
	CallerScopeSession   CallerScopeKind = "session"
)

// CallerScope is one concrete grant or command target: the whole owner, one
// Workspace and its Sessions, or one Session.
type CallerScope struct {
	Kind      CallerScopeKind
	Workspace string
	SessionID string
}

// harnessScope is the whole-owner target, which is the same value as the
// whole-owner grant.
var harnessScope = CallerScope{Kind: CallerScopeHarness}

// CallerContext is the trusted host's grant input.
type CallerContext struct {
	Origin      harness.InputOrigin
	ExtensionID string // present exactly for plugin attribution
	Scope       CallerScope
}

// Caller is one trusted command attribution and target grant. It is issued
// only by the owner; request data never supplies its origin or scope.
type Caller struct {
	owner       *Melta
	origin      harness.InputOrigin
	extensionID string
	scope       CallerScope
}

// Caller validates and issues one trusted grant. Only the holder of the owner
// can mint one; the embedding application binds it after its own
// authentication.
func (r *Melta) Caller(grant CallerContext) (Caller, error) {
	return r.caller(grant)
}

// caller is the one owner-bound grant constructor: the public Caller method
// and the host-derived command Caller share the same concrete scope
// validation.
func (r *Melta) caller(grant CallerContext) (Caller, error) {
	if r == nil {
		return Caller{}, fmt.Errorf("caller requires an issuing owner: %w", harness.ErrInvalid)
	}
	c, err := newCaller(grant)
	if err != nil {
		return Caller{}, err
	}
	c.owner = r
	return c, nil
}

// newCaller validates one trusted grant: the origin is the closed
// user/runtime/plugin set with provenance exactly for plugin, and the scope
// is exactly one concrete target union member.
func newCaller(grant CallerContext) (Caller, error) {
	switch grant.Origin {
	case harness.InputOriginUser, harness.InputOriginRuntime:
		if grant.ExtensionID != "" {
			return Caller{}, fmt.Errorf("input origin %q must not carry extension attribution: %w", grant.Origin, harness.ErrInvalid)
		}
	case harness.InputOriginPlugin:
		if grant.ExtensionID == "" {
			return Caller{}, fmt.Errorf("plugin Caller requires extension attribution: %w", harness.ErrInvalid)
		}
	default:
		return Caller{}, fmt.Errorf("Caller origin %q is not one of user, runtime or plugin: %w", grant.Origin, harness.ErrInvalid)
	}
	scope := grant.Scope
	switch scope.Kind {
	case CallerScopeHarness:
		if scope.Workspace != "" || scope.SessionID != "" {
			return Caller{}, fmt.Errorf("harness scope carries no target identity: %w", harness.ErrInvalid)
		}
	case CallerScopeWorkspace:
		if scope.SessionID != "" || scope.Workspace == "" || !filepath.IsAbs(scope.Workspace) || filepath.Clean(scope.Workspace) != scope.Workspace {
			return Caller{}, fmt.Errorf("workspace scope requires one clean absolute path: %w", harness.ErrInvalid)
		}
	case CallerScopeSession:
		if scope.Workspace != "" || !canonicalSessionID(scope.SessionID) {
			return Caller{}, fmt.Errorf("session scope requires one canonical session id: %w", harness.ErrInvalid)
		}
	default:
		return Caller{}, fmt.Errorf("Caller scope %q is not one of harness, workspace or session: %w", scope.Kind, harness.ErrInvalid)
	}
	return Caller{origin: grant.Origin, extensionID: grant.ExtensionID, scope: scope}, nil
}

// authorize binds a producer target to this owner and its caller grant. A
// caller issued by a different owner, or the zero Caller, is denied before any
// target read.
func (r *Melta) authorize(c Caller, ctx context.Context, h *harness.Harness, target CallerScope) error {
	if c.owner != r {
		return fmt.Errorf("caller was not issued by this owner: %w", ErrDenied)
	}
	return c.authorize(ctx, h, target)
}

// authorize validates one producer target against the grant: harness scope
// reaches every target, a Workspace grant reaches that Workspace and its
// Sessions, and a Session grant reaches only itself. A Workspace grant
// targeting a Session resolves the Session's immutable Workspace through one
// authoritative read. Everything else is denied before the producer reads or
// transitions.
func (c Caller) authorize(ctx context.Context, h *harness.Harness, target CallerScope) error {
	switch target.Kind {
	case CallerScopeHarness:
		if c.scope.Kind == CallerScopeHarness {
			return nil
		}
	case CallerScopeWorkspace:
		switch c.scope.Kind {
		case CallerScopeHarness:
			return nil
		case CallerScopeWorkspace:
			if c.scope.Workspace == target.Workspace {
				return nil
			}
		}
	case CallerScopeSession:
		switch c.scope.Kind {
		case CallerScopeHarness:
			return nil
		case CallerScopeSession:
			if c.scope.SessionID == target.SessionID {
				return nil
			}
		case CallerScopeWorkspace:
			if h == nil {
				return fmt.Errorf("session target requires an owner read: %w", ErrDenied)
			}
			header, err := h.ReadSessionHeader(ctx, target.SessionID)
			if err != nil {
				return err
			}
			if header.Identity.Workspace == c.scope.Workspace {
				return nil
			}
		}
	}
	return fmt.Errorf("%s target is outside the Caller scope: %w", target.Kind, ErrDenied)
}

// canonicalSessionID reports whether value is exactly 32 lowercase hex
// digits.
func canonicalSessionID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}
