// Package servicetest provides tests to ensure that implementations of
// platform/task/backend.Store and platform/task/backend.LogReader meet the requirements of platform.TaskService.
//
// Consumers of this package must import query/builtin.
// This package does not import it directly, to avoid requiring it too early.
package servicetest

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	platform "github.com/influxdata/influxdb"
	icontext "github.com/influxdata/influxdb/context"
	"github.com/influxdata/influxdb/inmem"
	"github.com/influxdata/influxdb/task"
	"github.com/influxdata/influxdb/task/backend"
	"github.com/influxdata/influxdb/task/options"
)

func init() {
	// TODO(mr): remove as part of https://github.com/influxdata/platform/issues/484.
	options.EnableScriptCacheForTest()
}

// BackendComponentFactory is supplied by consumers of the adaptertest package,
// to provide the values required to constitute a PlatformAdapter.
// The provided context.CancelFunc is called after the test,
// and it is the implementer's responsibility to clean up after that is called.
//
// If creating the System fails, the implementer should call t.Fatal.
type BackendComponentFactory func(t *testing.T) (*System, context.CancelFunc)

// TestTaskService should be called by consumers of the servicetest package.
// This will call fn once to create a single platform.TaskService
// used across all subtests in TestTaskService.
func TestTaskService(t *testing.T, fn BackendComponentFactory) {
	sys, cancel := fn(t)
	defer cancel()
	if sys.TaskServiceFunc == nil {
		sys.ts = task.PlatformAdapter(sys.S, sys.LR, sys.Sch, sys.I)
	} else {
		sys.ts = sys.TaskServiceFunc()
	}

	t.Run("TaskService", func(t *testing.T) {
		// We're running the subtests in parallel, but if we don't use this wrapper,
		// the defer cancel() call above would return before the parallel subtests completed.
		//
		// Running the subtests in parallel might make them slightly faster,
		// but more importantly, it should exercise concurrency to catch data races.

		t.Run("Task CRUD", func(t *testing.T) {
			t.Parallel()
			testTaskCRUD(t, sys)
		})

		t.Run("Task Runs", func(t *testing.T) {
			t.Parallel()
			testTaskRuns(t, sys)
		})

		t.Run("Task Concurrency", func(t *testing.T) {
			if testing.Short() {
				t.Skip("skipping in short mode")
			}
			t.Parallel()
			testTaskConcurrency(t, sys)
		})

		t.Run("Task Meta Updates", func(t *testing.T) {
			t.Parallel()
			testMetaUpdate(t, sys)
		})
	})
}

// TestCreds encapsulates credentials needed for a system to properly work with tasks.
type TestCreds struct {
	OrgID, UserID, AuthorizationID platform.ID
	Token                          string
}

func (tc TestCreds) Authorizer() platform.Authorizer {
	return &platform.Authorization{
		ID:     tc.AuthorizationID,
		OrgID:  tc.OrgID,
		UserID: tc.UserID,
		Token:  tc.Token,
	}
}

// System, as in "system under test", encapsulates the required parts of a platform.TaskAdapter
// (the underlying Store, LogReader, and LogWriter) for low-level operations.
type System struct {
	S   backend.Store
	LR  backend.LogReader
	LW  backend.LogWriter
	Sch backend.Scheduler

	// Used in the Creds function to create valid organizations, users, tokens, etc.
	I *inmem.Service

	// Set this context, to be used in tests, so that any spawned goroutines watching Ctx.Done()
	// will clean up after themselves.
	Ctx context.Context

	// Override for how to create a TaskService.
	// Most callers should leave this nil, in which case the tests will call task.PlatformAdapter.
	TaskServiceFunc func() platform.TaskService

	// Override for accessing credentials for an individual test.
	// Callers can leave this nil and the test will create its own random IDs for each test.
	// However, if the system needs to verify credentials,
	// the caller should set this value and return valid IDs and a valid token.
	// It is safe if this returns the same values every time it is called.
	CredsFunc func() (TestCreds, error)

	// Underlying task service, initialized inside TestTaskService,
	// either by instantiating a PlatformAdapter directly or by calling TaskServiceFunc.
	ts platform.TaskService
}

func testTaskCRUD(t *testing.T, sys *System) {
	cr := creds(t, sys)
	authzID := cr.AuthorizationID

	// Create a task.
	tc := platform.TaskCreate{
		OrganizationID: cr.OrgID,
		Flux:           fmt.Sprintf(scriptFmt, 0),
		Token:          cr.Token,
	}

	authorizedCtx := icontext.SetAuthorizer(sys.Ctx, cr.Authorizer())

	task, err := sys.ts.CreateTask(authorizedCtx, tc)
	if err != nil {
		t.Fatal(err)
	}
	if !task.ID.Valid() {
		t.Fatal("no task ID set")
	}

	findTask := func(tasks []*platform.Task, id platform.ID) *platform.Task {
		for _, t := range tasks {
			if t.ID == id {
				return t
			}
		}
		t.Fatalf("failed to find task by id %s", id)
		return nil
	}

	// Look up a task the different ways we can.
	// Map of method name to found task.
	found := map[string]*platform.Task{
		"Created": task,
	}

	// Find by ID should return the right task.
	f, err := sys.ts.FindTaskByID(sys.Ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found["FindTaskByID"] = f

	fs, _, err := sys.ts.FindTasks(sys.Ctx, platform.TaskFilter{Organization: &cr.OrgID})
	if err != nil {
		t.Fatal(err)
	}
	f = findTask(fs, task.ID)
	found["FindTasks with Organization filter"] = f

	fs, _, err = sys.ts.FindTasks(sys.Ctx, platform.TaskFilter{User: &cr.UserID})
	if err != nil {
		t.Fatal(err)
	}

	f = findTask(fs, task.ID)
	found["FindTasks with User filter"] = f

	for fn, f := range found {
		if f.OrganizationID != cr.OrgID {
			t.Fatalf("%s: wrong organization returned; want %s, got %s", fn, cr.OrgID.String(), f.OrganizationID.String())
		}
		if f.AuthorizationID != authzID {
			t.Fatalf("%s: wrong authorization ID returned; want %s, got %s", fn, authzID.String(), f.AuthorizationID.String())
		}
		if f.Name != "task #0" {
			t.Fatalf(`%s: wrong name returned; want "task #0", got %q`, fn, f.Name)
		}
		if f.Cron != "* * * * *" {
			t.Fatalf(`%s: wrong cron returned; want "* * * * *", got %q`, fn, f.Cron)
		}
		if f.Offset != "5s" {
			t.Fatalf(`%s: wrong offset returned; want "5s", got %q`, fn, f.Offset)
		}
		if f.Every != "" {
			t.Fatalf(`%s: wrong every returned; want "", got %q`, fn, f.Every)
		}
		if f.Status != string(backend.DefaultTaskStatus) {
			t.Fatalf(`%s: wrong default task status; want %q, got %q`, fn, backend.DefaultTaskStatus, f.Status)
		}
	}

	// Update task: script only.
	newFlux := fmt.Sprintf(scriptFmt, 99)
	origID := f.ID
	f, err = sys.ts.UpdateTask(authorizedCtx, origID, platform.TaskUpdate{Flux: &newFlux})
	if err != nil {
		t.Fatal(err)
	}

	if origID != f.ID {
		t.Fatalf("task ID unexpectedly changed during update, from %s to %s", origID.String(), f.ID.String())
	}
	if f.Flux != newFlux {
		t.Fatalf("wrong flux from update; want %q, got %q", newFlux, f.Flux)
	}
	if f.Status != string(backend.TaskActive) {
		t.Fatalf("expected task to be created active, got %q", f.Status)
	}

	// Update task: status only.
	newStatus := string(backend.TaskInactive)
	f, err = sys.ts.UpdateTask(authorizedCtx, origID, platform.TaskUpdate{Status: &newStatus})
	if err != nil {
		t.Fatal(err)
	}
	if f.Flux != newFlux {
		t.Fatalf("flux unexpected updated: %s", f.Flux)
	}
	if f.Status != newStatus {
		t.Fatalf("expected task status to be inactive, got %q", f.Status)
	}

	// Update task: reactivate status and update script.
	newStatus = string(backend.TaskActive)
	newFlux = fmt.Sprintf(scriptFmt, 98)
	f, err = sys.ts.UpdateTask(authorizedCtx, origID, platform.TaskUpdate{Flux: &newFlux, Status: &newStatus})
	if err != nil {
		t.Fatal(err)
	}
	if f.Flux != newFlux {
		t.Fatalf("flux unexpected updated: %s", f.Flux)
	}
	if f.Status != newStatus {
		t.Fatalf("expected task status to be inactive, got %q", f.Status)
	}

	// Update task: just update an option.
	newStatus = string(backend.TaskActive)
	newFlux = fmt.Sprintf(scriptDifferentName, 98)
	f, err = sys.ts.UpdateTask(authorizedCtx, origID, platform.TaskUpdate{Options: options.Options{Name: "task-changed #98"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Flux != newFlux {
		diff := cmp.Diff(f.Flux, newFlux)
		t.Fatalf("flux unexpected updated: %s", diff)
	}
	if f.Status != newStatus {
		t.Fatalf("expected task status to be active, got %q", f.Status)
	}

	// Update task: just update the token.
	// First we need to make a new authorization in order to get a new token.
	newAuthz := &platform.Authorization{OrgID: cr.OrgID, UserID: cr.UserID}
	if err := sys.I.CreateAuthorization(sys.Ctx, newAuthz); err != nil {
		t.Fatal(err)
	}
	f, err = sys.ts.UpdateTask(authorizedCtx, origID, platform.TaskUpdate{Token: newAuthz.Token})
	if err != nil {
		t.Fatal(err)
	}
	if f.AuthorizationID != newAuthz.ID {
		t.Fatalf("expected authorization ID %v, got %v", newAuthz.ID, f.AuthorizationID)
	}

	// Delete task.
	if err := sys.ts.DeleteTask(sys.Ctx, origID); err != nil {
		t.Fatal(err)
	}

	// Task should not be returned.
	if _, err := sys.ts.FindTaskByID(sys.Ctx, origID); err != backend.ErrTaskNotFound {
		t.Fatalf("expected %v, got %v", backend.ErrTaskNotFound, err)
	}
}

func testMetaUpdate(t *testing.T, sys *System) {
	cr := creds(t, sys)

	now := time.Now()
	ct := platform.TaskCreate{
		OrganizationID: cr.OrgID,
		Flux:           fmt.Sprintf(scriptFmt, 0),
		Token:          cr.Token,
	}
	authorizedCtx := icontext.SetAuthorizer(sys.Ctx, cr.Authorizer())
	task, err := sys.ts.CreateTask(authorizedCtx, ct)
	if err != nil {
		t.Fatal(err)
	}

	st, err := sys.ts.FindTaskByID(sys.Ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	after := time.Now()
	earliestCA := now.Add(-time.Second)
	latestCA := after.Add(time.Second)

	ca, err := time.Parse(time.RFC3339, st.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	if earliestCA.After(ca) || latestCA.Before(ca) {
		t.Fatalf("createdAt not accurate, expected %s to be between %s and %s", ca, earliestCA, latestCA)
	}

	ti, err := time.Parse(time.RFC3339, st.LatestCompleted)
	if err != nil {
		t.Fatal(err)
	}

	if now.Sub(ti) > 10*time.Second {
		t.Fatalf("latest completed not accurate, expected: ~%s, got %s", now, ti)
	}

	requestedAtUnix := time.Now().Add(5 * time.Minute).UTC().Unix()

	rc, err := sys.S.CreateNextRun(sys.Ctx, task.ID, requestedAtUnix)
	if err != nil {
		t.Fatal(err)
	}

	if err := sys.S.FinishRun(sys.Ctx, task.ID, rc.Created.RunID); err != nil {
		t.Fatal(err)
	}

	st2, err := sys.ts.FindTaskByID(sys.Ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	if st2.LatestCompleted <= st.LatestCompleted {
		t.Fatalf("executed task has not updated latest complete: expected > %s", st2.LatestCompleted)
	}

	now = time.Now()
	flux := fmt.Sprintf(scriptFmt, 1)
	task, err = sys.ts.UpdateTask(authorizedCtx, task.ID, platform.TaskUpdate{Flux: &flux})
	if err != nil {
		t.Fatal(err)
	}
	after = time.Now()

	earliestUA := now.Add(-time.Second)
	latestUA := after.Add(time.Second)

	ua, err := time.Parse(time.RFC3339, task.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}

	if earliestUA.After(ua) || latestUA.Before(ua) {
		t.Fatalf("updatedAt not accurate, expected %s to be between %s and %s", ua, earliestUA, latestUA)
	}

	st, err = sys.ts.FindTaskByID(sys.Ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	ua, err = time.Parse(time.RFC3339, st.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}

	if earliestUA.After(ua) || latestUA.Before(ua) {
		t.Fatalf("updatedAt not accurate after pulling new task, expected %s to be between %s and %s", ua, earliestUA, latestUA)
	}
}

func testTaskRuns(t *testing.T, sys *System) {
	cr := creds(t, sys)

	t.Run("FindRuns and FindRunByID", func(t *testing.T) {
		t.Parallel()

		// Script is set to run every minute. The platform adapter is currently hardcoded to schedule after "now",
		// which makes timing of runs somewhat difficult.
		ct := platform.TaskCreate{
			OrganizationID: cr.OrgID,
			Flux:           fmt.Sprintf(scriptFmt, 0),
			Token:          cr.Token,
		}
		task, err := sys.ts.CreateTask(icontext.SetAuthorizer(sys.Ctx, cr.Authorizer()), ct)
		if err != nil {
			t.Fatal(err)
		}
		st, err := sys.S.FindTaskByID(sys.Ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}

		requestedAtUnix := time.Now().Add(5 * time.Minute).UTC().Unix() // This should guarantee we can make two runs.

		rc0, err := sys.S.CreateNextRun(sys.Ctx, task.ID, requestedAtUnix)
		if err != nil {
			t.Fatal(err)
		}
		if rc0.Created.TaskID != task.ID {
			t.Fatalf("wrong task ID on created task: got %s, want %s", rc0.Created.TaskID, task.ID)
		}

		startedAt := time.Now().UTC()

		// Update the run state to Started; normally the scheduler would do this.
		rlb0 := backend.RunLogBase{
			Task:            st,
			RunID:           rc0.Created.RunID,
			RunScheduledFor: rc0.Created.Now,
			RequestedAt:     requestedAtUnix,
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb0, startedAt, backend.RunStarted); err != nil {
			t.Fatal(err)
		}

		rc1, err := sys.S.CreateNextRun(sys.Ctx, task.ID, requestedAtUnix)
		if err != nil {
			t.Fatal(err)
		}
		if rc1.Created.TaskID != task.ID {
			t.Fatalf("wrong task ID on created task: got %s, want %s", rc1.Created.TaskID, task.ID)
		}

		// Update the run state to Started; normally the scheduler would do this.
		rlb1 := backend.RunLogBase{
			Task:            st,
			RunID:           rc1.Created.RunID,
			RunScheduledFor: rc1.Created.Now,
			RequestedAt:     requestedAtUnix,
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb1, startedAt, backend.RunStarted); err != nil {
			t.Fatal(err)
		}
		// Mark the second run finished.
		if err := sys.S.FinishRun(sys.Ctx, task.ID, rlb1.RunID); err != nil {
			t.Fatal(err)
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb1, startedAt.Add(time.Second), backend.RunSuccess); err != nil {
			t.Fatal(err)
		}

		// Limit 1 should only return the earlier run.
		runs, _, err := sys.ts.FindRuns(sys.Ctx, platform.RunFilter{Org: &cr.OrgID, Task: &task.ID, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			t.Fatalf("expected 1 run, got %v", runs)
		}
		if runs[0].ID != rc0.Created.RunID {
			t.Fatalf("retrieved wrong run ID; want %s, got %s", rc0.Created.RunID, runs[0].ID)
		}
		if exp := startedAt.Format(time.RFC3339Nano); runs[0].StartedAt != exp {
			t.Fatalf("unexpectedStartedAt; want %s, got %s", exp, runs[0].StartedAt)
		}
		if runs[0].Status != backend.RunStarted.String() {
			t.Fatalf("unexpected run status; want %s, got %s", backend.RunStarted.String(), runs[0].Status)
		}
		if runs[0].FinishedAt != "" {
			t.Fatalf("expected empty FinishedAt, got %q", runs[0].FinishedAt)
		}

		// Unspecified limit returns both runs.
		runs, _, err = sys.ts.FindRuns(sys.Ctx, platform.RunFilter{Org: &cr.OrgID, Task: &task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 {
			t.Fatalf("expected 2 runs, got %v", runs)
		}
		if runs[0].ID != rc0.Created.RunID {
			t.Fatalf("retrieved wrong run ID; want %s, got %s", rc0.Created.RunID, runs[0].ID)
		}
		if exp := startedAt.Format(time.RFC3339Nano); runs[0].StartedAt != exp {
			t.Fatalf("unexpectedStartedAt; want %s, got %s", exp, runs[0].StartedAt)
		}
		if runs[0].Status != backend.RunStarted.String() {
			t.Fatalf("unexpected run status; want %s, got %s", backend.RunStarted.String(), runs[0].Status)
		}
		if runs[0].FinishedAt != "" {
			t.Fatalf("expected empty FinishedAt, got %q", runs[0].FinishedAt)
		}

		if runs[1].ID != rc1.Created.RunID {
			t.Fatalf("retrieved wrong run ID; want %s, got %s", rc1.Created.RunID, runs[1].ID)
		}
		if runs[1].StartedAt != runs[0].StartedAt {
			t.Fatalf("unexpected StartedAt; want %s, got %s", runs[0].StartedAt, runs[1].StartedAt)
		}
		if runs[1].Status != backend.RunSuccess.String() {
			t.Fatalf("unexpected run status; want %s, got %s", backend.RunSuccess.String(), runs[0].Status)
		}
		if exp := startedAt.Add(time.Second).Format(time.RFC3339Nano); runs[1].FinishedAt != exp {
			t.Fatalf("unexpected FinishedAt; want %s, got %s", exp, runs[1].FinishedAt)
		}

		// Look for a run that doesn't exist.
		_, err = sys.ts.FindRunByID(sys.Ctx, task.ID, platform.ID(math.MaxUint64))
		if err == nil {
			t.Fatalf("expected %s but got %s instead", backend.ErrRunNotFound, err)
		}

		// look for a taskID that doesn't exist.
		_, err = sys.ts.FindRunByID(sys.Ctx, platform.ID(math.MaxUint64), runs[0].ID)
		if err == nil {
			t.Fatalf("expected %s but got %s instead", backend.ErrRunNotFound, err)
		}

		foundRun0, err := sys.ts.FindRunByID(sys.Ctx, task.ID, runs[0].ID)
		if err != nil {
			t.Fatal(err)
		}

		if diff := cmp.Diff(foundRun0, runs[0]); diff != "" {
			t.Fatalf("difference between listed run and found run: %s", diff)
		}

		foundRun1, err := sys.ts.FindRunByID(sys.Ctx, task.ID, runs[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(foundRun1, runs[1]); diff != "" {
			t.Fatalf("difference between listed run and found run: %s", diff)
		}
	})

	t.Run("RetryRun", func(t *testing.T) {
		t.Parallel()

		// Script is set to run every minute. The platform adapter is currently hardcoded to schedule after "now",
		// which makes timing of runs somewhat difficult.
		ct := platform.TaskCreate{
			OrganizationID: cr.OrgID,
			Flux:           fmt.Sprintf(scriptFmt, 0),
			Token:          cr.Token,
		}
		task, err := sys.ts.CreateTask(icontext.SetAuthorizer(sys.Ctx, cr.Authorizer()), ct)
		if err != nil {
			t.Fatal(err)
		}
		st, err := sys.S.FindTaskByID(sys.Ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}

		// Non-existent ID should return the right error.
		_, err = sys.ts.RetryRun(sys.Ctx, task.ID, platform.ID(math.MaxUint64))
		if err != backend.ErrRunNotFound {
			t.Errorf("expected retrying run that doesn't exist to return %v, got %v", backend.ErrRunNotFound, err)
		}

		requestedAtUnix := time.Now().Add(5 * time.Minute).UTC().Unix() // This should guarantee we can make a run.

		rc, err := sys.S.CreateNextRun(sys.Ctx, task.ID, requestedAtUnix)
		if err != nil {
			t.Fatal(err)
		}
		if rc.Created.TaskID != task.ID {
			t.Fatalf("wrong task ID on created task: got %s, want %s", rc.Created.TaskID, task.ID)
		}

		startedAt := time.Now().UTC()

		// Update the run state to Started then Failed; normally the scheduler would do this.
		rlb := backend.RunLogBase{
			Task:            st,
			RunID:           rc.Created.RunID,
			RunScheduledFor: rc.Created.Now,
			RequestedAt:     requestedAtUnix,
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb, startedAt, backend.RunStarted); err != nil {
			t.Fatal(err)
		}
		if err := sys.S.FinishRun(sys.Ctx, task.ID, rlb.RunID); err != nil {
			t.Fatal(err)
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb, startedAt.Add(time.Second), backend.RunFail); err != nil {
			t.Fatal(err)
		}

		// Now retry the run.
		m, err := sys.ts.RetryRun(sys.Ctx, task.ID, rlb.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if m.TaskID != task.ID {
			t.Fatalf("wrong task ID on retried run: got %s, want %s", m.TaskID, task.ID)
		}
		if m.Status != "scheduled" {
			t.Fatal("expected new retried run to have status of scheduled")
		}
		nowTime, err := time.Parse(time.RFC3339, m.ScheduledFor)
		if err != nil {
			t.Fatalf("expected scheduledFor to be a parsable time in RFC3339, but got %s", m.ScheduledFor)
		}
		if nowTime.Unix() != rc.Created.Now {
			t.Fatalf("wrong scheduledFor on task: got %s, want %s", m.ScheduledFor, time.Unix(rc.Created.Now, 0).Format(time.RFC3339))
		}

		// Ensure the retry is added on the store task meta.
		meta, err := sys.S.FindTaskMetaByID(sys.Ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}

		found := false
		for _, mr := range meta.ManualRuns {
			if mr.Start == mr.End && mr.Start == rc.Created.Now {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("didn't find matching manual run after successful RetryRun call; got: %v", meta.ManualRuns)
		}

		exp := backend.RequestStillQueuedError{Start: rc.Created.Now, End: rc.Created.Now}

		// Retrying a run which has been queued but not started, should be rejected.
		if _, err = sys.ts.RetryRun(sys.Ctx, task.ID, rlb.RunID); err != exp {
			t.Fatalf("subsequent retry should have been rejected with %v; got %v", exp, err)
		}
	})

	t.Run("ForceRun", func(t *testing.T) {
		t.Parallel()

		ct := platform.TaskCreate{
			OrganizationID: cr.OrgID,
			Flux:           fmt.Sprintf(scriptFmt, 0),
			Token:          cr.Token,
		}
		task, err := sys.ts.CreateTask(icontext.SetAuthorizer(sys.Ctx, cr.Authorizer()), ct)
		if err != nil {
			t.Fatal(err)
		}

		beforeForce := time.Now().Unix()
		const scheduledFor = 77
		forcedRun, err := sys.ts.ForceRun(sys.Ctx, task.ID, scheduledFor)
		if err != nil {
			t.Fatal(err)
		}
		afterForce := time.Now().Unix()

		m, err := sys.S.FindTaskMetaByID(sys.Ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}

		if len(m.ManualRuns) != 1 {
			t.Fatalf("expected 1 manual run created, got: %#v", m.ManualRuns)
		}

		if rid := platform.ID(m.ManualRuns[0].RunID); rid != forcedRun.ID {
			t.Fatalf("expected run ID %s, got %s", forcedRun.ID, rid)
		}

		if requestedAt := m.ManualRuns[0].RequestedAt; requestedAt < beforeForce || requestedAt > afterForce {
			t.Fatalf("expected run RequestedAt to be in [%d, %d]; got %d", beforeForce, afterForce, requestedAt)
		}

		exp := backend.RequestStillQueuedError{Start: scheduledFor, End: scheduledFor}

		// Forcing the same run before it's executed should be rejected.
		if _, err = sys.ts.ForceRun(sys.Ctx, task.ID, scheduledFor); err != exp {
			t.Fatalf("subsequent force should have been rejected with %v; got %v", exp, err)
		}
	})

	t.Run("FindLogs", func(t *testing.T) {
		t.Parallel()

		ct := platform.TaskCreate{
			OrganizationID: cr.OrgID,
			Flux:           fmt.Sprintf(scriptFmt, 0),
			Token:          cr.Token,
		}
		task, err := sys.ts.CreateTask(icontext.SetAuthorizer(sys.Ctx, cr.Authorizer()), ct)
		if err != nil {
			t.Fatal(err)
		}
		st, err := sys.S.FindTaskByID(sys.Ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}

		requestedAtUnix := time.Now().Add(5 * time.Minute).UTC().Unix() // This should guarantee we can make a run.

		// Create two runs.
		rc1, err := sys.S.CreateNextRun(sys.Ctx, task.ID, requestedAtUnix)
		if err != nil {
			t.Fatal(err)
		}
		rlb1 := backend.RunLogBase{
			Task:            st,
			RunID:           rc1.Created.RunID,
			RunScheduledFor: rc1.Created.Now,
			RequestedAt:     requestedAtUnix,
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb1, time.Now(), backend.RunStarted); err != nil {
			t.Fatal(err)
		}

		rc2, err := sys.S.CreateNextRun(sys.Ctx, task.ID, requestedAtUnix)
		if err != nil {
			t.Fatal(err)
		}
		rlb2 := backend.RunLogBase{
			Task:            st,
			RunID:           rc2.Created.RunID,
			RunScheduledFor: rc2.Created.Now,
			RequestedAt:     requestedAtUnix,
		}
		if err := sys.LW.UpdateRunState(sys.Ctx, rlb2, time.Now(), backend.RunStarted); err != nil {
			t.Fatal(err)
		}

		// Add a log for the first run.
		log1Time := time.Now().UTC()
		if err := sys.LW.AddRunLog(sys.Ctx, rlb1, log1Time, "entry 1"); err != nil {
			t.Fatal(err)
		}

		// Ensure it is returned when filtering logs by run ID.
		logs, err := sys.LR.ListLogs(sys.Ctx, platform.LogFilter{
			Org:  &cr.OrgID,
			Task: &task.ID,
			Run:  &rc1.Created.RunID,
		})
		if err != nil {
			t.Fatal(err)
		}

		expLine1 := platform.Log(log1Time.Format(time.RFC3339Nano) + ": entry 1")
		exp := []platform.Log{expLine1}
		if diff := cmp.Diff(logs, exp); diff != "" {
			t.Fatalf("unexpected log: -got/+want: %s", diff)
		}

		// Add a log for the second run.
		log2Time := time.Now().UTC()
		if err := sys.LW.AddRunLog(sys.Ctx, rlb2, log2Time, "entry 2"); err != nil {
			t.Fatal(err)
		}

		// Ensure both returned when filtering logs by task ID.
		logs, err = sys.LR.ListLogs(sys.Ctx, platform.LogFilter{
			Org:  &cr.OrgID,
			Task: &task.ID,
		})
		if err != nil {
			t.Fatal(err)
		}

		expLine2 := platform.Log(log2Time.Format(time.RFC3339Nano) + ": entry 2")
		exp = []platform.Log{expLine1, expLine2}
		if diff := cmp.Diff(logs, exp); diff != "" {
			t.Fatalf("unexpected log: -got/+want: %s", diff)
		}
	})
}

func testTaskConcurrency(t *testing.T, sys *System) {
	cr := creds(t, sys)

	const numTasks = 450 // Arbitrarily chosen to get a reasonable count of concurrent creates and deletes.
	createTaskCh := make(chan platform.TaskCreate, numTasks)

	// Since this test is run in parallel with other tests,
	// we need to keep a whitelist of IDs that are okay to delete.
	// This only matters when the creds function returns an identical user/org from another test.
	var idMu sync.Mutex
	taskIDs := make(map[platform.ID]struct{})

	var createWg sync.WaitGroup
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		createWg.Add(1)
		go func() {
			defer createWg.Done()
			aCtx := icontext.SetAuthorizer(sys.Ctx, cr.Authorizer())
			for ct := range createTaskCh {
				task, err := sys.ts.CreateTask(aCtx, ct)
				if err != nil {
					t.Errorf("error creating task: %v", err)
					continue
				}
				idMu.Lock()
				taskIDs[task.ID] = struct{}{}
				idMu.Unlock()
			}
		}()
	}

	// Signal for non-creator goroutines to stop.
	quitCh := make(chan struct{})
	go func() {
		createWg.Wait()
		close(quitCh)
	}()

	var extraWg sync.WaitGroup
	// Get all the tasks, and delete the first one we find.
	extraWg.Add(1)
	go func() {
		defer extraWg.Done()

		deleted := 0
		defer func() {
			t.Logf("Concurrently deleted %d tasks", deleted)
		}()
		for {
			// Check if we need to quit.
			select {
			case <-quitCh:
				return
			default:
			}

			// Get all the tasks.
			tasks, _, err := sys.ts.FindTasks(sys.Ctx, platform.TaskFilter{Organization: &cr.OrgID})
			if err != nil {
				t.Errorf("error finding tasks: %v", err)
				return
			}
			if len(tasks) == 0 {
				continue
			}

			// Check again if we need to quit.
			select {
			case <-quitCh:
				return
			default:
			}

			for _, tsk := range tasks {
				// Was the retrieved task an ID we're allowed to delete?
				idMu.Lock()
				_, ok := taskIDs[tsk.ID]
				idMu.Unlock()
				if !ok {
					continue
				}

				// Task was in whitelist. Delete it from the TaskService.
				// We could remove it from the taskIDs map, but this test is short-lived enough
				// that clearing out the map isn't really worth taking the lock again.
				if err := sys.ts.DeleteTask(sys.Ctx, tsk.ID); err != nil {
					t.Errorf("error deleting task: %v", err)
					return
				}
				deleted++

				// Wait just a tiny bit.
				time.Sleep(time.Millisecond)
				break
			}
		}
	}()

	extraWg.Add(1)
	go func() {
		defer extraWg.Done()

		runsCreated := 0
		defer func() {
			t.Logf("Concurrently created %d runs", runsCreated)
		}()
		for {
			// Check if we need to quit.
			select {
			case <-quitCh:
				return
			default:
			}

			// Get all the tasks.
			tasks, _, err := sys.ts.FindTasks(sys.Ctx, platform.TaskFilter{Organization: &cr.OrgID})
			if err != nil {
				t.Errorf("error finding tasks: %v", err)
				return
			}
			if len(tasks) == 0 {
				continue
			}

			// Check again if we need to quit.
			select {
			case <-quitCh:
				return
			default:
			}

			// Create a run for the last task we found.
			// The script should run every minute, so use max now.
			tid := tasks[len(tasks)-1].ID
			if _, err := sys.S.CreateNextRun(sys.Ctx, tid, math.MaxInt64); err != nil {
				// This may have errored due to the task being deleted. Check if the task still exists.
				if _, err2 := sys.S.FindTaskByID(sys.Ctx, tid); err2 == backend.ErrTaskNotFound {
					// It was deleted. Just continue.
					continue
				}
				// Otherwise, we were able to find the task, so something went wrong here.
				t.Errorf("error creating next run: %v", err)
				return
			}
			runsCreated++

			// Wait just a tiny bit.
			time.Sleep(time.Millisecond)
		}
	}()

	// Start adding tasks.
	for i := 0; i < numTasks; i++ {
		createTaskCh <- platform.TaskCreate{
			OrganizationID: cr.OrgID,
			Flux:           fmt.Sprintf(scriptFmt, i),
			Token:          cr.Token,
		}
	}

	// Done adding. Wait for cleanup.
	close(createTaskCh)
	createWg.Wait()
	extraWg.Wait()
}

func creds(t *testing.T, s *System) TestCreds {
	t.Helper()

	if s.CredsFunc == nil {
		u := &platform.User{Name: t.Name() + "-user"}
		if err := s.I.CreateUser(s.Ctx, u); err != nil {
			t.Fatal(err)
		}
		o := &platform.Organization{Name: t.Name() + "-org"}
		if err := s.I.CreateOrganization(s.Ctx, o); err != nil {
			t.Fatal(err)
		}

		if err := s.I.CreateUserResourceMapping(s.Ctx, &platform.UserResourceMapping{
			ResourceType: platform.OrgsResourceType,
			ResourceID:   o.ID,
			UserID:       u.ID,
			UserType:     platform.Owner,
		}); err != nil {
			t.Fatal(err)
		}

		authz := platform.Authorization{
			OrgID:       o.ID,
			UserID:      u.ID,
			Permissions: platform.OperPermissions(),
		}
		if err := s.I.CreateAuthorization(context.Background(), &authz); err != nil {
			t.Fatal(err)
		}

		return TestCreds{
			OrgID:           o.ID,
			UserID:          u.ID,
			AuthorizationID: authz.ID,
			Token:           authz.Token,
		}
	}

	c, err := s.CredsFunc()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const (
	scriptFmt = `import "http"

option task = {
	name: "task #%d",
	cron: "* * * * *",
	offset: 5s,
	concurrency: 100,
}

from(bucket:"b")
	|> http.to(url: "http://example.com")`

	scriptDifferentName = `import "http"

option task = {
	name: "task-changed #%d",
	cron: "* * * * *",
	offset: 5s,
	concurrency: 100,
}

from(bucket: "b")
	|> http.to(url: "http://example.com")`
)
