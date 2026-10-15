// Package clienthost connects one client host to a compatible authenticated
// Runtime owner, starting the runtime command when discovery holds no usable
// owner. It owns only launch, readiness waiting, and discovery: Runtime
// lifetime, projection state, and work stay with the caller.
package clienthost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/hostprocess"
)

// discoveryFileName is the one startup record name inside DataDir, the same
// fixed name the Runtime owner publishes.
const discoveryFileName = "runtime.json"

// probeTimeout bounds one readiness probe. The caller's context still wins:
// a probe-only timeout is unusable discovery, never a caller cancellation.
const probeTimeout = time.Second

// readinessInterval is the live-owner probe cadence while a started
// contender runs.
const readinessInterval = 250 * time.Millisecond

// restartDelay is the pause before discovery is restarted after a started
// contender exited before this client held a compatible owner.
const restartDelay = time.Second

// Options are one host's startup inputs. An omitted DataDir resolves to
// ~/.lightcode, an omitted ConfigPath to an absolute LIGHTCODE_CONFIG or the
// data directory's config.json, an omitted Executable to the running
// executable, and an omitted PrefixArgs to none; explicit members win.
type Options struct {
	DataDir    string
	ConfigPath string
	Executable string
	PrefixArgs []string
}

// DefaultOptions resolves the standard startup inputs for the current
// environment through the same resolution Connect applies to omitted members.
func DefaultOptions() (Options, error) {
	return resolve(Options{})
}

// Connect returns one compatible authenticated client over the resolved
// discovery record, starting the plain runtime command when no usable owner
// exists. An authenticated owner speaking another wire version is reported
// immediately. A started contender that exits before this client holds a
// compatible owner is replaced from discovery after one second whatever its
// exit status; its asynchronous join never makes caller cancellation kill it.
func Connect(ctx context.Context, opts Options) (*client.Client, error) {
	resolved, err := resolve(opts)
	if err != nil {
		return nil, err
	}
	discoveryPath := filepath.Join(resolved.DataDir, discoveryFileName)
	for {
		c, err := probe(ctx, discoveryPath)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, client.ErrNoRuntime) {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		done, err := startContender(resolved)
		if err != nil {
			return nil, err
		}
		c, err = awaitReadiness(ctx, discoveryPath, done)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, client.ErrNoRuntime) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(restartDelay):
		}
	}
}

// resolve applies the stated defaults once: an absolute DataDir, an absolute
// ConfigPath from the explicit member, the configured LIGHTCODE_CONFIG, or
// the data directory's config.json, and the running executable. The
// caller-owned prefix arguments pass through unchanged; startContender copies
// them into the fresh launched argv.
func resolve(opts Options) (Options, error) {
	dataDir := opts.DataDir
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Options{}, fmt.Errorf("resolve home directory: %w", err)
		}
		dataDir = filepath.Join(home, ".lightcode")
	}
	absoluteDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Options{}, fmt.Errorf("normalize data directory: %w", err)
	}
	opts.DataDir = absoluteDataDir

	configPath := opts.ConfigPath
	if configPath == "" {
		if configured := os.Getenv("LIGHTCODE_CONFIG"); configured != "" {
			configPath = configured
		} else {
			configPath = filepath.Join(absoluteDataDir, "config.json")
		}
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return Options{}, fmt.Errorf("normalize config path: %w", err)
	}
	opts.ConfigPath = absoluteConfigPath

	if opts.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return Options{}, fmt.Errorf("resolve executable: %w", err)
		}
		opts.Executable = executable
	}
	return opts, nil
}

// probe runs one bounded readiness read over the existing client Connect. A
// probe-only timeout is unusable discovery; a canceled caller context is
// preserved as its own error.
func probe(ctx context.Context, discoveryPath string) (*client.Client, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c, err := client.Connect(probeCtx, discoveryPath)
	if err == nil {
		return c, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("readiness probe timed out: %w", client.ErrNoRuntime)
	}
	return nil, err
}

// awaitReadiness probes a live contender's readiness every readinessInterval
// and returns the first compatible client. An exited contender triggers one
// fresh probe before its outcome is interpreted, so a winner already
// published takes precedence over this local process's exit; any other exit
// reports unusable discovery and restarts the algorithm.
func awaitReadiness(ctx context.Context, discoveryPath string, done <-chan struct{}) (*client.Client, error) {
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return probe(ctx, discoveryPath)
		case <-ticker.C:
			c, err := probe(ctx, discoveryPath)
			if err == nil {
				return c, nil
			}
			if !errors.Is(err, client.ErrNoRuntime) {
				return nil, err
			}
		}
	}
}

// startContender starts the plain detached runtime command and joins it
// asynchronously. Connect cancellation ends only its own probes and waits:
// the contender keeps running and its exit is observed as the closed done
// channel.
func startContender(opts Options) (<-chan struct{}, error) {
	args := make([]string, 0, len(opts.PrefixArgs)+5)
	args = append(args, opts.PrefixArgs...)
	args = append(args, "runtime", "--data-dir", opts.DataDir, "--config", opts.ConfigPath)
	cmd := exec.Command(opts.Executable, args...)
	cmd.SysProcAttr = hostprocess.DetachAttr()
	// Nil Stdin/Stdout/Stderr is the child's null device: the started owner
	// keeps no pipe or terminal lifetime of this host.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start runtime contender: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done, nil
}
