package jobs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// unknownIDText is the retained unknown-ID error for one job identity.
func unknownIDText(id string) string {
	return fmt.Sprintf("process: no process with ID %q", id)
}

var jobIDShape = regexp.MustCompile(`^[0-9a-f]{8}$`)

// openJobs opens one real jobs service over a fresh data root and registers
// its Close — which joins the monitor and callback work — with the test.
func openJobs(t *testing.T, dataDir string) *Manager {
	t.Helper()
	manager := New(dataDir)
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// TestManagerSatisfiesTheOwnerSeams proves the one construction supplies both
// consumers: the tools' jobs contract and the Harness job-stop seam.
func TestManagerSatisfiesTheOwnerSeams(t *testing.T) {
	var jobsValue any = New(t.TempDir())
	_, isJobs := jobsValue.(Jobs)
	_, isStopper := jobsValue.(interface{ StopJob(string, string) })
	testassert.Require(t, isJobs && isStopper, "Manager implements Jobs=%v job stopper=%v, want both", isJobs, isStopper)
}

// armTestReaper replaces the wait seam with one parked until the returned
// release function runs. Cleanup releases the barrier, closes the service so
// every reaper goroutine leaves the seam, and only then restores the seam.
func armTestReaper(t *testing.T, inst *Manager) (release func()) {
	t.Helper()
	orig := waitCommand
	gate := make(chan struct{})
	var once sync.Once
	waitCommand = func(cmd *exec.Cmd) error {
		<-gate
		return orig(cmd)
	}
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() {
		release()
		_ = inst.Close()
		waitCommand = orig
	})
	return release
}

// start runs one reserved start, failing the test on error.
func start(t *testing.T, j *Manager, req StartRequest) string {
	t.Helper()
	{
		err := j.Start(req)
		testassert.Require(t, err == nil, "Start(%q): %v", req.Command, err)
	}
	return req.JobID
}

// reserveStart reserves one identity and starts it.
func reserveStart(t *testing.T, j *Manager, req StartRequest) string {
	t.Helper()
	id, err := j.Reserve()
	testassert.NoError(t, err)
	req.JobID = id
	return start(t, j, req)
}

// waitExit waits for one gated exit callback's result.
func waitExit(t *testing.T, ch <-chan ExitResult) ExitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("the exit callback did not run")
		return ExitResult{}
	}
}

// waitRemoved polls until the identified record leaves the map, which the exit
// goroutine does after the callback and before its work-group Done.
func waitRemoved(t *testing.T, j *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j.mu.Lock()
		_, ok := j.jobs[id]
		j.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("record %q was not removed after the callback", id)
}

// stateOf reads one record's lifecycle state.
func stateOf(j *Manager, id string) (jobState, bool) {
	j.mu.Lock()
	rec, ok := j.jobs[id]
	j.mu.Unlock()
	if !ok {
		return "", false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.state, true
}

// TestStartReadKillListLiveRetainedTexts drives the tool-facing methods
// against real short-lived processes and asserts every retained text: the
// empty-read placeholder, the running and exited list lines, the empty list,
// completed/killed exit results, and the unknown-ID errors after removal. A
// second Start over a live record fails the retained unknown-ID error without
// removing it.
func TestStartReadKillListLiveRetainedTexts(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const sid = "session-live"

	emptyExit := make(chan ExitResult, 1)
	emptyID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "sleep 30", Env: os.Environ(),
		OnExit: func(r ExitResult) { emptyExit <- r },
	})
	testassert.Require(t, jobIDShape.MatchString(emptyID), "Reserve minted %q, want an 8-hex job ID", emptyID)
	got, err := j.Read(sid, emptyID)
	testassert.Require(t, err == nil && got == "(No output yet)", "Read of a silent running job = (%q, %v), want (No output yet)", got, err)
	line := j.List(sid)
	wantLine := fmt.Sprintf("%s  sleep 30  (running for ", emptyID)
	testassert.Require(t, strings.HasPrefix(line, wantLine) && strings.HasSuffix(line, ")") && strings.Count(line, "\n") == 0, "List = %q, want the single running line for %q without a trailing newline", line, emptyID)
	{
		got := j.Live(sid)
		testassert.Require(t, slices.Equal(got, []string{emptyID}), "Live = %v, want the running job", got)
	}
	// A second Start over an already-running record fails the retained
	// unknown-ID check and leaves the record in place.
	if err := j.Start(StartRequest{JobID: emptyID, SessionID: sid, Command: "printf hijack", Env: os.Environ()}); err == nil || err.Error() != unknownIDText(emptyID) {
		t.Fatalf("Start over a running record = %v, want %q", err, unknownIDText(emptyID))
	}
	{
		got := j.Live(sid)
		testassert.Require(t, slices.Equal(got, []string{emptyID}), "Live after the rejected Start = %v, want the record untouched", got)
	}

	exit := make(chan ExitResult, 1)
	shortID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "printf hello", Env: os.Environ(),
		OnExit: func(r ExitResult) { exit <- r },
	})
	result := waitExit(t, exit)
	testassert.Require(t, result.ID == shortID && result.Command == "printf hello" && result.Reason == "completed" && result.ExitCode == 0 && result.Output == "hello" && result.MaxOutputBytes == 15360, "completed ExitResult = %+v, want the short job's identity, completed reason, and exit code 0; got %v, want %v; got %v, want %v", result, result.Output, "hello", result.MaxOutputBytes, 15360)
	waitRemoved(t, j, shortID)
	if _, err := j.Read(sid, shortID); err == nil || err.Error() != unknownIDText(shortID) {
		t.Fatalf("Read after removal = %v, want %q", err, unknownIDText(shortID))
	}
	// The Start over an exited-but-retained record also fails unknown-ID: in
	// the gated window below the record exists with state exited.
	gatedExit := make(chan ExitResult, 1)
	gatedEntered := make(chan struct{})
	gatedRelease := make(chan struct{})
	gatedID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "printf bye", Env: os.Environ(),
		OnExit: func(r ExitResult) {
			close(gatedEntered)
			gatedExit <- r
			<-gatedRelease
		},
	})
	select {
	case <-gatedEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the gated exit callback did not run")
	}
	if err := j.Start(StartRequest{JobID: gatedID, SessionID: sid, Command: "printf x", Env: os.Environ()}); err == nil || err.Error() != unknownIDText(gatedID) {
		t.Fatalf("Start over an exited record = %v, want %q", err, unknownIDText(gatedID))
	}
	exitedLine := j.List(sid)
	wantExited := fmt.Sprintf("%s  printf bye  (running for ", gatedID)
	testassert.Require(t, strings.Contains(exitedLine, wantExited) && strings.Contains(exitedLine, " (exited with code 0))"), "List = %q, want the exited line for %q with the exited suffix", exitedLine, gatedID)
	{
		got, err := j.Read(sid, gatedID)
		testassert.Require(t, err == nil && got == "bye", "Read of the exited-but-retained record = (%q, %v), want the final output", got, err)
	}
	close(gatedRelease)
	waitExit(t, gatedExit)
	waitRemoved(t, j, gatedID)

	// Kill on the running silent job returns nil and delivers the killed
	// result; Kill on the absent short job reports the retained unknown ID.
	testassert.NoError(t, j.Kill(sid, emptyID))
	killed := waitExit(t, emptyExit)
	testassert.Require(t, killed.Reason == "killed", "killed ExitResult = %+v, want the killed reason", killed)
	waitRemoved(t, j, emptyID)
	if err := j.Kill(sid, shortID); err == nil || err.Error() != unknownIDText(shortID) {
		t.Fatalf("Kill after removal = %v, want %q", err, unknownIDText(shortID))
	}
	testassert.Equal(t, j.List(sid), "No background processes.")
	{
		got := j.Live(sid)
		testassert.Require(t, len(got) == 0, "Live = %v, want no jobs", got)
	}
}

// TestKillExitedShortCircuits proves the retained Kill contract over an
// exited record: nil while the record still exists, without disturbing the
// gated callback's own cleanup.
func TestKillExitedShortCircuits(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const sid = "session-kill-exited"
	entered := make(chan struct{})
	release := make(chan struct{})
	exit := make(chan ExitResult, 1)
	id := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "printf done", Env: os.Environ(),
		OnExit: func(r ExitResult) {
			close(entered)
			exit <- r
			<-release
		},
	})
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the gated exit callback did not run")
	}
	testassert.NoError(t, j.Kill(sid, id))
	close(release)
	waitExit(t, exit)
	waitRemoved(t, j, id)
}

// TestKillReservedIsUnknownIDWithoutRemoval proves Kill over a still-reserved
// identity reports the retained unknown-ID error and does not consume the
// reservation: the subsequent Start still succeeds.
func TestKillReservedIsUnknownIDWithoutRemoval(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const sid = "session-kill-reserved"
	id, err := j.Reserve()
	testassert.NoError(t, err)
	if err := j.Kill(sid, id); err == nil || err.Error() != unknownIDText(id) {
		t.Fatalf("Kill of a reserved job = %v, want %q", err, unknownIDText(id))
	}
	exit := make(chan ExitResult, 1)
	start(t, j, StartRequest{JobID: id, SessionID: sid, Command: "printf ok", Env: os.Environ(), OnExit: func(r ExitResult) { exit <- r }})
	waitExit(t, exit)
	waitRemoved(t, j, id)
}

// TestLiveReturnsSortedIDs proves Live alone guarantees the sorted order of
// the session's running job IDs.
func TestLiveReturnsSortedIDs(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const sid = "session-live-sorted"
	exits := make([]chan ExitResult, 0, 3)
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		exit := make(chan ExitResult, 1)
		exits = append(exits, exit)
		ids = append(ids, reserveStart(t, j, StartRequest{
			SessionID: sid, Command: "sleep 30", Env: os.Environ(),
			OnExit: func(r ExitResult) { exit <- r },
		}))
	}
	want := slices.Clone(ids)
	slices.Sort(want)
	{
		got := j.Live(sid)
		testassert.Require(t, slices.Equal(got, want), "Live = %v, want the sorted IDs %v", got, want)
	}
	for i, id := range ids {
		{
			err := j.Kill(sid, id)
			testassert.Require(t, err == nil, "Kill(%q): %v", id, err)
		}
		waitExit(t, exits[i])
		waitRemoved(t, j, id)
	}
}

// TestSessionAttributionHidesForeignJobs proves one session never observes
// another's records: read and kill yield the unknown-ID texts, list shows
// only its own lines, and Live excludes the foreign job.
func TestSessionAttributionHidesForeignJobs(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const (
		owner   = "session-owner"
		foreign = "session-foreign"
	)
	ownerExit := make(chan ExitResult, 1)
	ownerID := reserveStart(t, j, StartRequest{
		SessionID: owner, Command: "sleep 30", Env: os.Environ(),
		OnExit: func(r ExitResult) { ownerExit <- r },
	})
	foreignExit := make(chan ExitResult, 1)
	foreignID := reserveStart(t, j, StartRequest{
		SessionID: foreign, Command: "sleep 30", Env: os.Environ(),
		OnExit: func(r ExitResult) { foreignExit <- r },
	})

	if got, err := j.Read(foreign, ownerID); err == nil || err.Error() != unknownIDText(ownerID) {
		t.Fatalf("foreign Read = (%q, %v), want %q", got, err, unknownIDText(ownerID))
	}
	if err := j.Kill(foreign, ownerID); err == nil || err.Error() != unknownIDText(ownerID) {
		t.Fatalf("foreign Kill = %v, want %q", err, unknownIDText(ownerID))
	}
	{
		got := j.Live(foreign)
		testassert.Require(t, slices.Equal(got, []string{foreignID}), "foreign Live = %v, want only the foreign session's job", got)
	}
	{
		list := j.List(foreign)
		testassert.Require(t, !strings.Contains(list, ownerID) && strings.Contains(list, foreignID), "foreign List = %q, want only the foreign session's line", list)
	}
	{
		list := j.List(owner)
		testassert.Require(t, strings.Contains(list, ownerID) && !strings.Contains(list, foreignID), "owner List = %q, want only the owner session's line", list)
	}
	// The foreign Kill left the owner's job running.
	{
		got := j.Live(owner)
		testassert.Require(t, slices.Equal(got, []string{ownerID}), "owner Live after a foreign Kill = %v, want the job untouched", got)
	}

	testassert.NoError(t, j.Kill(owner, ownerID))
	waitExit(t, ownerExit)
	waitRemoved(t, j, ownerID)
	testassert.NoError(t, j.Kill(foreign, foreignID))
	waitExit(t, foreignExit)
	waitRemoved(t, j, foreignID)
}

// TestStartRejectedAtSessionLimit proves the cap error text at the cap, that
// the failed Start consumes its reservation, that exited records leave the
// count, and that the limit is per session.
func TestStartRejectedAtSessionLimit(t *testing.T) {
	j := openJobs(t, t.TempDir())
	cfg := []byte(`{"max_background_processes":1}`)
	const (
		first  = "session-limit-first"
		second = "session-limit-second"
	)
	firstExit := make(chan ExitResult, 1)
	firstID := reserveStart(t, j, StartRequest{
		SessionID: first, Command: "sleep 30", Env: os.Environ(), Config: cfg,
		OnExit: func(r ExitResult) { firstExit <- r },
	})

	blockedExit := make(chan ExitResult, 1)
	blockedID, err := j.Reserve()
	testassert.NoError(t, err)
	err = j.Start(StartRequest{
		JobID: blockedID, SessionID: first, Command: "sleep 30", Env: os.Environ(), Config: cfg,
		OnExit: func(r ExitResult) { blockedExit <- r },
	})
	wantErr := "process: background process limit reached (1/1). Kill existing processes or wait for them to exit"
	testassert.Require(t, err != nil && err.Error() == wantErr, "Start at the cap = %v, want %q", err, wantErr)
	// The limit failure consumed the reservation: the same identity fails the
	// retained unknown-ID error.
	err = j.Start(StartRequest{
		JobID: blockedID, SessionID: first, Command: "sleep 30", Env: os.Environ(), Config: cfg,
	})
	if err == nil || err.Error() != unknownIDText(blockedID) {
		t.Fatalf("Start after the consumed reservation = %v, want %q", err, unknownIDText(blockedID))
	}

	// Another session is unaffected by the first session's cap.
	otherExit := make(chan ExitResult, 1)
	otherID := reserveStart(t, j, StartRequest{
		SessionID: second, Command: "sleep 30", Env: os.Environ(), Config: cfg,
		OnExit: func(r ExitResult) { otherExit <- r },
	})

	// Once the first session's job exits it no longer counts against the cap.
	testassert.NoError(t, j.Kill(first, firstID))
	waitExit(t, firstExit)
	retryExit := make(chan ExitResult, 1)
	retryID := reserveStart(t, j, StartRequest{
		SessionID: first, Command: "sleep 30", Env: os.Environ(), Config: cfg,
		OnExit: func(r ExitResult) { retryExit <- r },
	})

	for sid, id := range map[string]string{first: retryID, second: otherID} {
		{
			err := j.Kill(sid, id)
			testassert.Require(t, err == nil, "Kill(%q): %v", id, err)
		}
	}
	waitExit(t, retryExit)
	waitExit(t, otherExit)
	waitRemoved(t, j, retryID)
	waitRemoved(t, j, otherID)
}

// TestValidateConfigSettingsRangesAndShapes covers the decoder contract
// directly: absent/blank/null/{} and null members decode on the defaults,
// unknown keys are ignored even with wrong types, every bound is exact, range
// failures name the jobs field and range, and malformed JSON, non-object
// input, and wrong consumed-field types wrap the retained invalid-settings
// text.
func TestValidateSettingsRangesAndShapes(t *testing.T) {
	validate := func(raw string) error {
		return ValidateSettings([]byte(raw))
	}
	for _, raw := range []string{
		"", " ", "{}", "null",
		`{"max_background_processes":null,"max_output_bytes":null,"read_line_max_chars":null}`,
		`{"max_background_processes":10,"max_output_bytes":15360,"read_line_max_chars":5000}`,
		// Exact lower and upper bounds.
		`{"max_background_processes":1}`, `{"max_background_processes":50}`,
		`{"max_output_bytes":1024}`, `{"max_output_bytes":1048576}`,
		`{"read_line_max_chars":100}`, `{"read_line_max_chars":100000}`,
		// Unknown keys are ignored regardless of type.
		`{"unknown_member":{"deep":[1,2]}}`,
		`{"unknown_member":"many","max_background_processes":5}`,
	} {
		{
			err := validate(raw)
			testassert.Check(t, err == nil, "ValidateSettings(%s) = %v, want accepted", raw, err)
		}
	}
	for _, row := range []struct{ raw, want string }{
		{`{"max_background_processes":0}`, "jobs: max_background_processes must be between 1 and 50"},
		{`{"max_background_processes":51}`, "jobs: max_background_processes must be between 1 and 50"},
		{`{"max_output_bytes":1023}`, "jobs: max_output_bytes must be between 1024 and 1048576"},
		{`{"max_output_bytes":1048577}`, "jobs: max_output_bytes must be between 1024 and 1048576"},
		{`{"read_line_max_chars":99}`, "jobs: read_line_max_chars must be between 100 and 100000"},
		{`{"read_line_max_chars":100001}`, "jobs: read_line_max_chars must be between 100 and 100000"},
	} {
		{
			err := validate(row.raw)
			testassert.Check(t, err != nil && err.Error() == row.want, "ValidateSettings(%s) = %v, want %q", row.raw, err, row.want)
		}
	}
	for _, raw := range []string{
		`{"max_output_bytes":"many"}`,
		`{"read_line_max_chars":true}`,
		`{"max_background_processes":1.5}`,
		`{not json`,
		`[]`, `"jobs"`, `5`,
	} {
		err := validate(raw)
		testassert.Check(t, err != nil && strings.HasPrefix(err.Error(), "jobs: invalid settings: "), "ValidateSettings(%s) = %v, want the jobs: invalid settings wrap", raw, err)
	}
}

// TestStartInvalidConfigConsumesReservation proves Start applies req.Config
// per call: an invalid per-call section fails with the decoder's text and
// consumes the reservation, while the same identity then fails unknown-ID.
func TestStartInvalidConfigConsumesReservation(t *testing.T) {
	j := openJobs(t, t.TempDir())
	for _, row := range []struct{ config, want string }{
		{`{"max_output_bytes":1023}`, "jobs: max_output_bytes must be between 1024 and 1048576"},
		{`{not json`, "jobs: invalid settings: "},
	} {
		id, err := j.Reserve()
		testassert.NoError(t, err)
		err = j.Start(StartRequest{
			JobID: id, SessionID: "session-bad-config", Command: "printf x", Env: os.Environ(),
			Config: []byte(row.config),
		})
		testassert.Require(t, err != nil && strings.HasPrefix(err.Error(), row.want), "Start with Config %s = %v, want prefix %q", row.config, err, row.want)
		err = j.Start(StartRequest{
			JobID: id, SessionID: "session-bad-config", Command: "printf x", Env: os.Environ(),
			Config: []byte(row.config),
		})
		if err == nil || err.Error() != unknownIDText(id) {
			t.Fatalf("Start after the consumed reservation = %v, want %q", err, unknownIDText(id))
		}
	}
}

// TestReserveAbortThenStartFailsUnknownID proves the reservation rule: Abort
// removes a reservation idempotently, the pending Start then fails the
// retained unknown-ID error, and Abort over a running record is a no-op.
func TestReserveAbortThenStartFailsUnknownID(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const sid = "session-abort"
	id, err := j.Reserve()
	testassert.NoError(t, err)
	testassert.Require(t, jobIDShape.MatchString(id), "Reserve minted %q, want an 8-hex job ID", id)
	j.Abort(id)
	j.Abort(id)         // idempotent
	j.Abort("00000000") // absent stays a no-op
	err = j.Start(StartRequest{JobID: id, SessionID: sid, Command: "printf x", Env: os.Environ()})
	if err == nil || err.Error() != unknownIDText(id) {
		t.Fatalf("Start after Abort = %v, want %q", err, unknownIDText(id))
	}
	// Abort over a running record resolves nothing: the job keeps running.
	exit := make(chan ExitResult, 1)
	runningID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "sleep 30", Env: os.Environ(),
		OnExit: func(r ExitResult) { exit <- r },
	})
	j.Abort(runningID)
	{
		got := j.Live(sid)
		testassert.Require(t, slices.Equal(got, []string{runningID}), "Live after Abort over a running record = %v, want the record untouched", got)
	}
	testassert.NoError(t, j.Kill(sid, runningID))
	waitExit(t, exit)
	waitRemoved(t, j, runningID)
}

// TestStopJobAcrossStates covers the total StopJob: a reserved identity is
// removed so the later Start fails unknown-ID, a running job converges
// through TERM/KILL reaping to the killed result, and exited, absent, or
// foreign identities return without effect.
func TestStopJobAcrossStates(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const (
		sid     = "session-stop"
		foreign = "session-stop-foreign"
	)

	// Reserved: removed; the pending Start fails the same unknown-ID error.
	reservedID, err := j.Reserve()
	testassert.NoError(t, err)
	j.StopJob(sid, reservedID)
	{
		_, ok := stateOf(j, reservedID)
		testassert.Require(t, !ok, "StopJob left the reservation in place")
	}
	err = j.Start(StartRequest{JobID: reservedID, SessionID: sid, Command: "printf x", Env: os.Environ()})
	if err == nil || err.Error() != unknownIDText(reservedID) {
		t.Fatalf("Start after StopJob removed the reservation = %v, want %q", err, unknownIDText(reservedID))
	}

	// Running: converges through the unbounded reaping path to the killed
	// result.
	runningExit := make(chan ExitResult, 1)
	runningID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "sleep 30", Env: os.Environ(),
		OnExit: func(r ExitResult) { runningExit <- r },
	})
	j.StopJob(sid, runningID)
	{
		got := waitExit(t, runningExit)
		testassert.Require(t, got.Reason == "killed", "StopJob exit result = %+v, want the killed reason", got)
	}
	waitRemoved(t, j, runningID)

	// Exited: returns; the gated callback's own completion is untouched.
	entered := make(chan struct{})
	release := make(chan struct{})
	exitedExit := make(chan ExitResult, 1)
	exitedID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "printf natural", Env: os.Environ(),
		OnExit: func(r ExitResult) {
			close(entered)
			exitedExit <- r
			<-release
		},
	})
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the gated exit callback did not run")
	}
	j.StopJob(sid, exitedID)
	close(release)
	{
		got := waitExit(t, exitedExit)
		testassert.Require(t, got.Reason == "completed", "StopJob over an exited record changed the result to %+v, want completed", got)
	}
	waitRemoved(t, j, exitedID)

	// Absent: returns.
	j.StopJob(sid, "00000000")

	// Foreign: returns and leaves the running job alone.
	ownerExit := make(chan ExitResult, 1)
	ownerID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "sleep 30", Env: os.Environ(),
		OnExit: func(r ExitResult) { ownerExit <- r },
	})
	j.StopJob(foreign, ownerID)
	{
		got := j.Live(sid)
		testassert.Require(t, slices.Equal(got, []string{ownerID}), "Live after a foreign StopJob = %v, want the job untouched", got)
	}
	j.StopJob(sid, ownerID)
	waitExit(t, ownerExit)
	waitRemoved(t, j, ownerID)
}

// TestTimeoutEscalatesToKill proves the timeout goroutine's retained
// escalation: a job that ignores SIGTERM still ends at its timeout through
// the SIGKILL leg, delivering the timeout reason well before the command's
// own lifetime.
func TestTimeoutEscalatesToKill(t *testing.T) {
	j := openJobs(t, t.TempDir())
	const sid = "session-timeout"
	exit := make(chan ExitResult, 1)
	id := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "trap '' TERM; while true; do sleep 0.05; done",
		Env: os.Environ(), TimeoutSec: 1,
		OnExit: func(r ExitResult) { exit <- r },
	})
	started := time.Now()
	result := waitExit(t, exit)
	{
		elapsed := time.Since(started)
		testassert.Require(t, elapsed <= 10*time.Second, "the timed job ended after %v, want the timeout escalation to converge promptly", elapsed)
	}
	testassert.Require(t, result.ID == id && result.Reason == "timeout", "timeout ExitResult = %+v, want the timeout reason", result)
	waitRemoved(t, j, id)
}

// TestCaptureBoundsSpillUnderSessionOutputDir proves the capture options:
// output beyond the configured bound formats with the retained truncation
// marker, the visible spill lands under the session artifact output
// directory with the proc_output_ prefix, and the job's own MaxOutputBytes
// bounds the composed completion.
func TestCaptureBoundsSpillUnderSessionOutputDir(t *testing.T) {
	dataDir := t.TempDir()
	j := openJobs(t, dataDir)
	const sid = "session-capture"
	entered := make(chan struct{})
	release := make(chan struct{})
	exit := make(chan ExitResult, 1)
	id := reserveStart(t, j, StartRequest{
		SessionID: sid,
		Command:   `i=0; while [ "$i" -lt 400 ]; do printf 'XXXXXXXXXXXXXXXXXXXX'; i=$((i+1)); done`,
		Env:       os.Environ(),
		Config:    []byte(`{"max_output_bytes":1024}`),
		OnExit: func(r ExitResult) {
			close(entered)
			exit <- r
			<-release
		},
	})
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the gated exit callback did not run")
	}
	// The record is retained while the callback is gated, so Read serves the
	// same bounded format the callback receives.
	wantDir := fmt.Sprintf("saved to: %s", filepath.Join(dataDir, "code", sid, "output"))
	got, err := j.Read(sid, id)
	testassert.NoError(t, err)
	testassert.Require(t, strings.Contains(got, "[Output truncated. Full output (") && strings.Contains(got, wantDir), "Read = %q, want the truncation marker and a spill under %s", got, wantDir)
	close(release)
	result := waitExit(t, exit)
	testassert.Require(t, result.MaxOutputBytes == 1024 && strings.Contains(result.Output, "[Output truncated. Full output (") && strings.Contains(result.Output, wantDir), "got %v, want %v; Output = %q, want the truncation marker and a spill under %s", result.MaxOutputBytes, 1024, result.Output, wantDir)
	waitRemoved(t, j, id)
	entries, err := os.ReadDir(filepath.Join(dataDir, "code", sid, "output"))
	testassert.NoError(t, err)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "proc_output_") && strings.HasSuffix(e.Name(), ".txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("session output directory holds %v, want a proc_output_ visible spill", names(entries))
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestCloseKillsStraysAndGatesCallbacks proves the scope Close contract: a
// callback admitted before Close blocks Close until released, a stray still
// running is killed and its not-yet-admitted callback is suppressed, capture
// and record cleanup completes before Close returns, and new reservations and
// starts fail the retained closed error.
func TestCloseKillsStraysAndGatesCallbacks(t *testing.T) {
	dataDir := t.TempDir()
	j := openJobs(t, dataDir)
	const sid = "session-close"

	// Job A exits first so its callback is admitted before Close.
	aEntered := make(chan struct{})
	aRelease := make(chan struct{})
	aExit := make(chan ExitResult, 1)
	aID := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "printf a", Env: os.Environ(),
		OnExit: func(r ExitResult) {
			close(aEntered)
			aExit <- r
			<-aRelease
		},
	})
	select {
	case <-aEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the admitted exit callback did not run")
	}

	// Job B is the stray: it overflows a tight capture bound, creating a
	// private spill, and keeps running until Close kills it.
	bExit := make(chan ExitResult, 1)
	bID := reserveStart(t, j, StartRequest{
		SessionID: sid,
		Command:   `i=0; while [ "$i" -lt 200 ]; do printf 'XXXXXXXXXXXXXXXXXXXX'; i=$((i+1)); done; sleep 30`,
		Env:       os.Environ(),
		Config:    []byte(`{"max_output_bytes":1024}`),
		OnExit:    func(r ExitResult) { bExit <- r },
	})
	outputDir := filepath.Join(dataDir, "code", sid, "output")
	deadline := time.Now().Add(15 * time.Second)
	sawSpill := false
	for time.Now().Before(deadline) {
		if entries, err := os.ReadDir(outputDir); err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".tmp") {
					sawSpill = true
				}
			}
		}
		if sawSpill {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	testassert.Require(t, sawSpill, "no private spill appeared under %s before Close", outputDir)

	closeDone := make(chan error, 1)
	go func() { closeDone <- j.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the admitted callback was released: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(aRelease)
	waitExit(t, aExit)
	select {
	case err := <-closeDone:
		testassert.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not converge after the admitted callback was released")
	}

	// The stray's not-yet-admitted callback was suppressed: no result arrived
	// even though the exit goroutine completed inside Close.
	select {
	case r := <-bExit:
		t.Fatalf("the not-yet-admitted callback ran with %+v, want suppression", r)
	default:
	}
	// Capture and record cleanup completed before Close returned.
	j.mu.Lock()
	remaining := make([]string, 0, len(j.jobs))
	for id := range j.jobs {
		remaining = append(remaining, id)
	}
	j.mu.Unlock()
	testassert.Require(t, len(remaining) == 0, "records remaining after Close = %v, want none", remaining)
	if entries, err := os.ReadDir(outputDir); err != nil {
		t.Fatalf("ReadDir after Close: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("capture files remaining after Close = %v, want none", names(entries))
	}
	{
		_, ok := stateOf(j, aID)
		testassert.Require(t, !ok, "record %q survived Close", aID)
	}
	{
		_, ok := stateOf(j, bID)
		testassert.Require(t, !ok, "record %q survived Close", bID)
	}
	{
		_, err := j.Reserve()
		testassert.Require(t, err != nil && err.Error() == "process: manager is closed", "Reserve after Close = %v, want the retained closed error", err)
	}
	err := j.Start(StartRequest{JobID: "deadbeef", SessionID: sid, Command: "printf x", Env: os.Environ()})
	testassert.Require(t, err != nil && err.Error() == "process: manager is closed", "Start after Close = %v, want the retained closed error", err)
}

// TestKillRetainsFiveSecondDiagnoseAndAbandon proves the user-facing Kill
// path keeps the legacy 5-second bound: with reaping held past SIGKILL, Kill
// diagnoses on stderr and returns nil instead of waiting forever.
func TestKillRetainsFiveSecondDiagnoseAndAbandon(t *testing.T) {
	j := openJobs(t, t.TempDir())
	release := armTestReaper(t, j)
	const sid = "session-kill-abandon"
	id := reserveStart(t, j, StartRequest{
		SessionID: sid, Command: "sleep 30", Env: os.Environ(),
	})

	pipeR, pipeW, err := os.Pipe()
	testassert.NoError(t, err)
	old := os.Stderr
	os.Stderr = pipeW
	started := time.Now()
	killErr := j.Kill(sid, id)
	elapsed := time.Since(started)
	os.Stderr = old
	_ = pipeW.Close()
	diagnosis, _ := io.ReadAll(pipeR)
	_ = pipeR.Close()

	testassert.Require(t, killErr == nil, "Kill = %v, want nil from the abandon outcome", killErr)
	testassert.Require(t, elapsed >= 5*time.Second && elapsed <= 15*time.Second, "Kill returned after %v, want the retained 5-second diagnose-and-abandon bound; Kill returned after %v, want no unbounded wait", elapsed, elapsed)
	want := fmt.Sprintf("lightcode: process %s not reaped within 5s after SIGKILL\n", id)
	testassert.Require(t, strings.Contains(string(diagnosis), want), "Kill stderr = %q, want %q", diagnosis, want)
	release() // the exit goroutine reaps during the registered cleanup
}
