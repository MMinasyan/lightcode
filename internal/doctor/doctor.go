// Package doctor runs read-only diagnostics over a Lightcode install. It
// composes only read-only seams — config.Parse/ReadDotEnvKeys/
// ClassifyKeySource, catalog.BuildOffline/ProviderConnected, the
// package-level project lookups, and lockfile reads — and performs zero
// filesystem writes and zero network access. It never prints; callers
// render the Report.
package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	agentcfg "github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/server"
)

// Check statuses.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// Check is one diagnostic result line.
type Check struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Report is the stable machine-readable result shape.
type Report struct {
	Checks   []Check `json:"checks"`
	OK       int     `json:"ok"`
	Warnings int     `json:"warnings"`
	Failures int     `json:"failures"`
}

// Params carries the environmental inputs so Run stays free of
// process-global reads that tests cannot control.
type Params struct {
	Home          string // user home dir; ignored when HomeErr is set
	HomeErr       error
	ConfigPath    string // resolved config path; ignored when ConfigPathErr is set
	ConfigPathErr error
	WorkDir       string // project lookup key (the process cwd)
	BinaryPath    string
	Version       string
	Platform      string
	LookupEnv     func(string) (string, bool) // process env; os.LookupEnv in production
}

// Run executes every check group. The second result reports that no
// provider is connected, which the human renderer turns into the
// fresh-install appendix line.
func Run(p Params) (Report, bool) {
	if p.LookupEnv == nil {
		p.LookupEnv = func(string) (string, bool) { return "", false }
	}
	var checks []Check
	add := func(group, name, status, detail string) {
		checks = append(checks, Check{Group: group, Name: name, Status: status, Detail: detail})
	}

	// Install.
	add("install", "binary", StatusOK, p.BinaryPath)
	add("install", "version", StatusOK, p.Version)
	add("install", "platform", StatusOK, p.Platform)
	if p.ConfigPathErr != nil {
		add("install", "config-path", StatusFail, fmt.Sprintf("resolve config path: %v", p.ConfigPathErr))
	} else {
		add("install", "config-path", StatusOK, p.ConfigPath)
	}
	if p.HomeErr != nil {
		add("install", "home", StatusFail, fmt.Sprintf("resolve home dir: %v", p.HomeErr))
		return summarize(checks), false
	}

	dataDir := filepath.Join(p.Home, ".lightcode")
	cfg, agentsCfg, dotenvKeys := dataChecks(p, dataDir, &checks, add)

	// Catalog.
	var userRaw map[string]any
	if cfg != nil {
		userRaw = cfg.Providers
	}
	result, err := catalog.BuildOffline(p.Home, userRaw)
	if err != nil {
		add("catalog", "build", StatusFail, err.Error())
		return summarize(checks), false
	}
	catalogChecks(result.Warnings, len(result.Catalog.Providers), add)

	// Providers. The connection resolver mirrors the agent's effective
	// environment after LoadDotEnv: shell presence (even an empty export)
	// shadows .env, and a credential resolves only when the effective
	// value is non-empty — a template-style `KEY=` line defines the name
	// but connects nothing.
	envIsSet := func(name string) bool {
		if shellVal, shellPresent := p.LookupEnv(name); shellPresent {
			return shellVal != ""
		}
		return dotenvKeys[name]
	}
	anyConnected := providerChecks(result.Catalog, dotenvKeys, p.LookupEnv, envIsSet, add)

	// Setup — WARN-level mirrors of the agent's setup warnings.
	if anyConnected {
		add("setup", "provider", StatusOK, "at least one provider is connected")
	} else {
		add("setup", "provider", StatusWarn, "no provider connected - configure a provider with credentials and at least one usable model")
	}
	if agentsCfg != nil {
		setupModelCheck(agentsCfg, result.Catalog, envIsSet, add)
	}

	daemonChecks(p, add)

	return summarize(checks), !anyConnected
}

// dataChecks runs the data-dir group and returns the parsed config (nil when
// absent or unparseable) and the .env key names, each mapped to whether its
// value is non-empty.
func dataChecks(p Params, dataDir string, checks *[]Check, add func(group, name, status, detail string)) (*config.Config, *agentcfg.Config, map[string]bool) {
	dotenvKeys := map[string]bool{}

	info, err := os.Lstat(dataDir)
	if os.IsNotExist(err) {
		add("data", "dir", StatusOK, "not created yet (created on first run)")
		return nil, nil, dotenvKeys
	}
	if err != nil {
		add("data", "dir", StatusFail, err.Error())
		return nil, nil, dotenvKeys
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		add("data", "dir", StatusWarn, dataDir+" is a symlink")
	case !info.IsDir():
		add("data", "dir", StatusWarn, dataDir+" is not a directory")
	case info.Mode().Perm()&^0o700 != 0:
		add("data", "dir", StatusWarn, fmt.Sprintf("permissions %04o, want 0700 or tighter", info.Mode().Perm()))
	default:
		add("data", "dir", StatusOK, dataDir)
	}

	var cfg *config.Config
	var agentsCfg *agentcfg.Config
	if p.ConfigPathErr == nil {
		if _, err := os.Stat(p.ConfigPath); os.IsNotExist(err) {
			add("data", "config", StatusOK, "config.json: not created yet (created on first run)")
		} else if err != nil {
			add("data", "config", StatusFail, err.Error())
		} else if data, err := os.ReadFile(p.ConfigPath); err != nil {
			add("data", "config", StatusFail, err.Error())
		} else if parsed, err := config.Parse(data); err != nil {
			add("data", "config", StatusFail, err.Error())
		} else {
			cfg = parsed
			add("data", "config", StatusOK, p.ConfigPath)
		}

		agentsPath := agentcfg.PathForConfig(p.ConfigPath)
		if _, err := os.Stat(agentsPath); os.IsNotExist(err) {
			if parsed, parseErr := agentcfg.Parse([]byte("{}")); parseErr == nil {
				agentsCfg = parsed
			}
			add("data", "agents", StatusOK, "agents.json: not created yet (created on first run)")
		} else if err != nil {
			add("data", "agents", StatusFail, err.Error())
		} else if data, err := os.ReadFile(agentsPath); err != nil {
			add("data", "agents", StatusFail, err.Error())
		} else if parsed, err := agentcfg.Parse(data); err != nil {
			add("data", "agents", StatusFail, err.Error())
		} else {
			agentsCfg = parsed
			add("data", "agents", StatusOK, agentsPath)
		}
	}

	envPath := filepath.Join(dataDir, ".env")
	if info, err := os.Lstat(envPath); os.IsNotExist(err) {
		add("data", "env", StatusOK, ".env: not created yet (created on first run)")
	} else if err != nil {
		add("data", "env", StatusWarn, err.Error())
	} else {
		if info.Mode().Perm()&^0o600 != 0 {
			add("data", "env", StatusWarn, fmt.Sprintf("permissions %04o, want 0600 or tighter", info.Mode().Perm()))
		}
		keys, malformed, err := config.ReadDotEnvKeys(envPath)
		if err != nil {
			add("data", "env", StatusWarn, err.Error())
		} else {
			dotenvKeys = keys
			for _, m := range malformed {
				add("data", "env", StatusWarn, m)
			}
			add("data", "env", StatusOK, fmt.Sprintf("%d keys", len(keys)))
		}
	}
	return cfg, agentsCfg, dotenvKeys
}

// catalogChecks maps build warnings to severities. A silently dropped user
// provider is the worst current failure mode, so user_config_skip is FAIL;
// everything else, including unknown kinds, is WARN and never dropped.
func catalogChecks(warnings []catalog.Warning, providerCount int, add func(group, name, status, detail string)) {
	if len(warnings) == 0 {
		add("catalog", "build", StatusOK, fmt.Sprintf("%d providers", providerCount))
		return
	}
	for _, w := range warnings {
		name := w.Provider
		if name == "" {
			name = "build"
		}
		switch w.Kind {
		case "user_config_skip":
			add("catalog", name, StatusFail, w.Message)
		case "bundled_config_skip", "incomplete_model", "empty_provider", "discovery_failure":
			add("catalog", name, StatusWarn, w.Message)
		default:
			add("catalog", name, StatusWarn, fmt.Sprintf("unknown warning kind %q: %s", w.Kind, w.Message))
		}
	}
}

func providerChecks(cat *catalog.Catalog, dotenvKeys map[string]bool, lookupEnv func(string) (string, bool), envIsSet func(string) bool, add func(group, name, status, detail string)) bool {
	ids := make([]string, 0, len(cat.Providers))
	for id := range cat.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	anyConnected := false
	// Agent-parity key-source adapters: external means a non-empty shell
	// value (an empty export classifies none, as the agent's Getenv check
	// does); managed means the name is defined in .env and not shadowed by
	// the shell — LoadDotEnv marks even empty .env values managed, but
	// skips keys the shell already set.
	external := func(name string) bool { shellVal, _ := lookupEnv(name); return shellVal != "" }
	managed := func(name string) bool {
		if _, shellPresent := lookupEnv(name); shellPresent {
			return false
		}
		_, inDotEnv := dotenvKeys[name]
		return inDotEnv
	}
	for _, id := range ids {
		prov := cat.Providers[id]
		keySource := config.ClassifyKeySource(prov.Transport.APIKeyEnv, external, managed)
		connected := catalog.ProviderConnected(prov, envIsSet)
		if connected {
			anyConnected = true
		}
		usable := 0
		for _, m := range prov.Models {
			if m != nil && m.ContextWindow > 0 {
				usable++
			}
		}
		state := "not connected"
		if connected {
			state = "connected"
		}
		detail := fmt.Sprintf("key %s, %s, %d usable models", keySource, state, usable)
		if keySource == config.KeySourceNone {
			add("providers", id, StatusWarn, fmt.Sprintf("no key in env or .env (set %s); %s, %d usable models", prov.Transport.APIKeyEnv, state, usable))
			continue
		}
		add("providers", id, StatusOK, detail)
	}
	return anyConnected
}

// setupModelCheck mirrors the agent's no-model / model-unavailable
// unavailable warnings, which are mutually exclusive.
func setupModelCheck(agentsCfg *agentcfg.Config, cat *catalog.Catalog, envIsSet func(string) bool, add func(group, name, status, detail string)) {
	resolved, err := agentsCfg.Resolve("primary", agentcfg.ResolveContext{})
	if err != nil {
		add("setup", "model", StatusWarn, err.Error())
		return
	}
	if resolved.Model == "" {
		add("setup", "model", StatusWarn, "No model is configured. Select a model to get started.")
		return
	}
	ref, err := catalog.ParseModelRef(resolved.Model)
	if err != nil {
		add("setup", "model", StatusWarn, fmt.Sprintf("configured model %q is unavailable: %v", resolved.Model, err))
		return
	}
	prov, model, err := cat.Lookup(ref)
	if err != nil || model == nil || model.ContextWindow <= 0 {
		add("setup", "model", StatusWarn, fmt.Sprintf("configured model %q is unavailable: not in the catalog or incomplete", resolved.Model))
		return
	}
	if !catalog.ProviderConnected(prov, envIsSet) {
		add("setup", "model", StatusWarn, fmt.Sprintf("configured model %q is unavailable: provider %s is not connected", resolved.Model, ref.Provider))
		return
	}
	add("setup", "model", StatusOK, resolved.Model)
}

func daemonChecks(p Params, add func(group, name, status, detail string)) {
	projectsRoot := filepath.Join(p.Home, ".lightcode", "projects")
	current, err := project.FindByPath(projectsRoot, p.WorkDir)
	switch {
	case err != nil:
		add("daemon", "project", StatusWarn, err.Error())
	case current == nil:
		add("daemon", "project", StatusOK, "no project record yet")
	default:
		add("daemon", "project", StatusOK, fmt.Sprintf("project %s (%s)", current.Name, current.ID))
	}

	lockPath := server.Path(p.Home)
	lf, err := server.Read(p.Home)
	if os.IsNotExist(err) {
		add("daemon", "lockfiles", StatusOK, "none")
		return
	}
	if err != nil {
		add("daemon", "owner", StatusWarn, fmt.Sprintf("malformed lockfile: %v", err))
		return
	}
	if server.IsStale(lf) {
		add("daemon", "owner", StatusWarn, fmt.Sprintf("stale lockfile (pid %d not running)", lf.PID))
		return
	}
	add("daemon", "owner", StatusOK, fmt.Sprintf("running (pid %d, port %d, lock %s)", lf.PID, lf.Port, lockPath))
}

func summarize(checks []Check) Report {
	r := Report{Checks: checks}
	for _, c := range checks {
		switch c.Status {
		case StatusOK:
			r.OK++
		case StatusWarn:
			r.Warnings++
		case StatusFail:
			r.Failures++
		}
	}
	return r
}
