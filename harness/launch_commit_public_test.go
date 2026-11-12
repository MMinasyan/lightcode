package harness_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/storage"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
)

func TestLaunchChildCommitPointOwnsAdmittedChild(t *testing.T) {
	rows := []struct {
		name          string
		cancelBefore  bool
		cancelHarness bool
		wantErr       bool
		wantChild     bool
		wantStatus    harness.OperationState
	}{
		{name: "caller canceled before commit", cancelBefore: true, wantErr: true},
		{name: "caller canceled after commit", wantChild: true, wantStatus: harness.OperationFailure},
		{name: "caller and harness canceled after commit", cancelHarness: true, wantErr: true, wantChild: true, wantStatus: harness.OperationInterruption},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			inner, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "melta.db"))
			testassert.NoError(t, err)
			t.Cleanup(func() { testassert.NoError(t, inner.Close()) })
			store := &publicationCancelStore{Storage: inner}
			f := newPublicFixtureWithJobs(t, store, nil, nil, func(context.Context, model.Request) (model.Stream, error) {
				return nil, errors.New("scripted child model failure")
			}, nil)
			t.Cleanup(func() { testassert.NoError(t, converge(t, f)) })
			ctx := context.Background()
			session, err := f.h.CreateSession(ctx, harness.CreateSessionRequest{Workspace: "/tmp/works", AgentSelection: publicSelection("coder")})
			testassert.NoError(t, err)
			launchCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if row.cancelBefore {
				cancel()
			} else {
				store.afterCommit = func() {
					cancel()
					if row.cancelHarness {
						f.cancel()
					}
				}
			}
			res, lerr := f.h.LaunchChildSession(launchCtx, harness.LaunchChildRequest{ParentSessionID: session.Identity.SessionID, AgentSelection: publicSelection("child-agent"), Content: []model.ContentPart{{Kind: model.PartText, Text: "child task"}}, OperationID: "child-op", ExtensionID: "test.extension", MaxConcurrent: 4, OutputLimit: 4096})
			testassert.Equal(t, lerr != nil, row.wantErr)
			testassert.Equal(t, res.ChildSessionID != "", row.wantChild && !row.wantErr)
			ids, err := inner.ListSessionIDs(ctx)
			testassert.NoError(t, err)
			child := ""
			for _, id := range ids {
				if id != session.Identity.SessionID {
					child = id
				}
			}
			testassert.Equal(t, child != "", row.wantChild)
			if !row.wantChild {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				op, err := f.h.ReadOperation(ctx, child, "child-op")
				testassert.NoError(t, err)
				status := op.State.Status
				if status == row.wantStatus {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("child operation status = %q, want %q", status, row.wantStatus)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
