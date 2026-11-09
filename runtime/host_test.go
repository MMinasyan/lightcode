package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/melta"
)

func testOwnerOptions(t *testing.T) melta.Options {
	t.Helper()
	dataDir := t.TempDir()
	return melta.Options{
		DataDir:       dataDir,
		Configuration: melta.NewValueConfigurationSource([]byte(`{}`)),
		Credentials:   melta.NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
	}
}

func testCaller(t *testing.T, owner *melta.Melta) melta.Caller {
	t.Helper()
	caller, err := owner.Caller(melta.CallerContext{Origin: harness.InputOriginUser, Scope: melta.CallerScope{Kind: melta.CallerScopeHarness}})
	testassert.NoError(t, err)
	return caller
}

// TestBeforeReleaseRunsAfterSettlementAndBeforeStoreRelease proves the one
// host withdrawal boundary on both explicit Close and owner-context
// cancellation: the callback observes the still-held ownership lock, runs
// before the store closer, and runs exactly once.
func TestBeforeReleaseRunsAfterSettlementAndBeforeStoreRelease(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		name := "lifetime cancellation"
		if explicit {
			name = "explicit close"
		}
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			var mu sync.Mutex
			var trace []string
			record := func(value string) {
				mu.Lock()
				trace = append(trace, value)
				mu.Unlock()
			}
			var storeCloses atomic.Int32
			options := melta.Options{
				DataDir:       dataDir,
				Configuration: melta.NewValueConfigurationSource([]byte(`{}`)),
				Credentials:   melta.NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
				OpenStore: func(ctx context.Context, _ string) (harness.Storage, func() error, error) {
					return storage.NewMemory(), func() error {
						record("store_close")
						storeCloses.Add(1)
						return nil
					}, nil
				},
				BeforeRelease: func() error {
					lock, ok, err := atomicfs.TryAcquire(filepath.Join(dataDir, "runtime.lock"))
					if err == nil && ok {
						_ = lock.Release()
						record("before_release:lock_free")
					} else {
						record("before_release:lock_held")
					}
					return nil
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			owner, err := melta.Open(ctx, options)
			testassert.NoError(t, err)
			if explicit {
				testassert.NoError(t, owner.Close(context.Background()))
				cancel()
			} else {
				cancel()
				testassert.NoError(t, owner.Close(context.Background()))
			}
			mu.Lock()
			got := append([]string(nil), trace...)
			mu.Unlock()
			testassert.Require(t, strings.Join(got, ",") == "before_release:lock_held,store_close", "release trace = %v, want the withdrawal while the lock is held before the store closes", got)
			testassert.Equal(t, storeCloses.Load(), int32(1))
		})
	}
}

// TestOpenStoreOwnsNamespaceAcrossRoots proves the supplied factory owns
// exclusivity of its actual namespace rather than the local data directory:
// one namespace refuses a second root while the first is live, and each
// acquired closer releases that namespace exactly once.
func TestOpenStoreOwnsNamespaceAcrossRoots(t *testing.T) {
	type namespace struct {
		mu       sync.Mutex
		owner    bool
		releases int
	}
	shared := &namespace{}
	factory := func(context.Context, string) (harness.Storage, func() error, error) {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		if shared.owner {
			return nil, nil, errors.New("external store namespace already owned")
		}
		shared.owner = true
		return storage.NewMemory(), func() error {
			shared.mu.Lock()
			defer shared.mu.Unlock()
			shared.owner = false
			shared.releases++
			return nil
		}, nil
	}
	open := func() (*melta.Melta, error) {
		options := testOwnerOptions(t)
		options.OpenStore = factory
		return melta.Open(context.Background(), options)
	}
	first, err := open()
	testassert.NoError(t, err)
	{
		_, err := open()
		testassert.Require(t, err != nil, "a second data root acquired the same external store namespace")
	}
	testassert.NoError(t, first.Close(context.Background()))
	shared.mu.Lock()
	releases := shared.releases
	shared.mu.Unlock()
	testassert.Equal(t, releases, 1)
	second, err := open()
	testassert.NoError(t, err)
	testassert.NoError(t, second.Close(context.Background()))
	shared.mu.Lock()
	releases = shared.releases
	shared.mu.Unlock()
	testassert.Equal(t, releases, 2)
}

// TestEmbeddedSubscribe proves the observation handle is usable without any
// HTTP host: one published reload reaches a direct Subscription.
func TestEmbeddedSubscribe(t *testing.T) {
	options := testOwnerOptions(t)
	source := options.Configuration
	owner, err := melta.Open(context.Background(), options)
	testassert.NoError(t, err)
	defer func() { _ = owner.Close(context.Background()) }()
	sub, err := owner.Subscribe(8)
	testassert.NoError(t, err)
	defer sub.Close()
	testassert.NoError(t, source.Write(context.Background(), []byte(`{"sessions":{"archive_after_days":9}}`)))
	revision, err := owner.Reload(context.Background(), testCaller(t, owner))
	testassert.Require(t, err == nil && revision.Generation == "2", "Reload = (%q, %v), want generation 2", revision, err)
	select {
	case event := <-sub.Events():
		kind, err := event.Discriminator()
		testassert.NoError(t, err)
		testassert.Equal(t, kind, "configuration_changed")
	case <-time.After(10 * time.Second):
		t.Fatal("no configuration event reached the embedded subscription")
	}
}

// blockingConfiguration parks one later Read until its gate closes while
// construction reads flow through.
type blockingConfiguration struct {
	melta.ConfigurationSource
	block   atomic.Bool
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (s *blockingConfiguration) Read(ctx context.Context) ([]byte, error) {
	if s.block.Load() {
		s.once.Do(func() { close(s.entered) })
		<-s.gate
	}
	return s.ConfigurationSource.Read(ctx)
}

// TestBeforeReleaseWaitsForBlockedSettlement proves the callback is not
// invoked while admitted work is still settling, then runs once after it.
func TestBeforeReleaseWaitsForBlockedSettlement(t *testing.T) {
	source := &blockingConfiguration{
		ConfigurationSource: melta.NewValueConfigurationSource([]byte(`{}`)),
		entered:             make(chan struct{}),
		gate:                make(chan struct{}),
	}
	var released atomic.Bool
	options := testOwnerOptions(t)
	options.Configuration = source
	options.BeforeRelease = func() error {
		released.Store(true)
		return nil
	}
	owner, err := melta.Open(context.Background(), options)
	testassert.NoError(t, err)

	source.block.Store(true)
	reloadDone := make(chan error, 1)
	go func() {
		_, err := owner.Reload(context.Background(), testCaller(t, owner))
		reloadDone <- err
	}()
	<-source.entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		probe, err := owner.Subscribe(1)
		if err != nil {
			break
		}
		probe.Close()
		testassert.Require(t, !time.Now().After(deadline), "owner shutdown never began")
		time.Sleep(time.Millisecond)
	}
	testassert.Require(t, !released.Load(), "BeforeRelease ran while admitted work was still settling")
	close(source.gate)
	_ = <-reloadDone
	testassert.NoError(t, <-closeDone)
	testassert.Require(t, released.Load(), "BeforeRelease did not run after settlement")
}

// TestBeforeReleaseErrorJoinsAndStillReleases proves a callback error joins
// Close while the store closer and ownership-lock release still happen.
func TestBeforeReleaseErrorJoinsAndStillReleases(t *testing.T) {
	errCallback := errors.New("host withdrawal failure")
	dataDir := t.TempDir()
	var storeCloses atomic.Int32
	options := melta.Options{
		DataDir:       dataDir,
		Configuration: melta.NewValueConfigurationSource([]byte(`{}`)),
		Credentials:   melta.NewEnvironmentCredentialSource(filepath.Join(dataDir, ".env")),
		OpenStore: func(context.Context, string) (harness.Storage, func() error, error) {
			return storage.NewMemory(), func() error {
				storeCloses.Add(1)
				return nil
			}, nil
		},
		BeforeRelease: func() error { return errCallback },
	}
	owner, err := melta.Open(context.Background(), options)
	testassert.NoError(t, err)
	err = owner.Close(context.Background())
	testassert.Require(t, errors.Is(err, errCallback), "Close = %v, want the BeforeRelease error joined", err)
	testassert.Equal(t, storeCloses.Load(), int32(1))
	lock, ok, err := atomicfs.TryAcquire(filepath.Join(dataDir, "runtime.lock"))
	testassert.Require(t, err == nil && ok, "ownership lock after Close = (%v, %v), want released", ok, err)
	testassert.NoError(t, lock.Release())
}
