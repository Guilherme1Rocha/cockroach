// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package jobs_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobstest"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/lease"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness/slstorage"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestRoundtripJob(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{})
	registry := s.JobRegistry().(*jobs.Registry)
	defer s.Stopper().Stop(ctx)

	jobID := registry.MakeJobID()
	record := jobs.Record{
		Description:   "beep boop",
		Username:      username.MakeSQLUsernameFromPreNormalizedString("robot"),
		DescriptorIDs: descpb.IDs{42},
		Details:       jobspb.RestoreDetails{},
		Progress:      jobspb.RestoreProgress{},
	}
	storedJob, err := registry.CreateAdoptableJobWithTxn(ctx, record, jobID, nil /* txn */)
	require.NoError(t, err)
	retrievedJob, err := registry.LoadJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if e, a := storedJob, retrievedJob; !reflect.DeepEqual(e, a) {
		//diff := strings.Join(pretty.Diff(e, a), "\n")
		t.Fatalf("stored job did not match retrieved job:\n%+v\n%+v", e, a)
	}
}

// TestExpiringSessionsAndClaimJobsDoesNotTouchTerminalJobs will ensure that we do not
// update the claim_session_id field of jobs when expiring sessions or claiming
// jobs.
func TestExpiringSessionsAndClaimJobsDoesNotTouchTerminalJobs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Don't adopt, cancel rapidly.
	adopt := 10 * time.Hour
	cancel := 10 * time.Millisecond
	args := base.TestServerArgs{Knobs: base.TestingKnobs{
		JobsTestingKnobs: jobs.NewTestingKnobsWithIntervals(adopt, cancel, adopt, adopt),
	}}

	ctx := context.Background()
	s, sqlDB, _ := serverutils.StartServer(t, args)
	defer s.Stopper().Stop(ctx)

	tdb := sqlutils.MakeSQLRunner(sqlDB)
	const insertQuery = `
   INSERT
     INTO system.jobs (
                        status,
                        claim_session_id,
                        claim_instance_id
                      )
   VALUES ($1, $2, $3)
RETURNING id;
`
	// Disallow clean up of claimed jobs
	jobs.CancellationsUpdateLimitSetting.Override(ctx, &s.ClusterSettings().SV, 0)
	terminalStates := []jobs.State{jobs.StateSucceeded, jobs.StateCanceled, jobs.StateFailed}
	terminalIDs := make([]jobspb.JobID, len(terminalStates))
	terminalClaims := make([][]byte, len(terminalStates))
	mkSessionID := func() []byte {
		sessionID, err := slstorage.MakeSessionID([]byte("us"), uuid.MakeV4())
		require.NoError(t, err)
		return []byte(sessionID)
	}
	for i, s := range terminalStates {
		terminalClaims[i] = mkSessionID() // bogus claim
		tdb.QueryRow(t, insertQuery, s, terminalClaims[i], 42).Scan(&terminalIDs[i])
	}
	var nonTerminalID jobspb.JobID
	tdb.QueryRow(t, insertQuery, jobs.StateRunning, mkSessionID(), 42).Scan(&nonTerminalID)

	checkClaimEqual := func(id jobspb.JobID, exp []byte) error {
		const getClaimQuery = `SELECT claim_session_id FROM system.jobs WHERE id = $1`
		var claim []byte
		tdb.QueryRow(t, getClaimQuery, id).Scan(&claim)
		if !bytes.Equal(claim, exp) {
			return errors.Errorf("expected nil, got %s", hex.EncodeToString(exp))
		}
		return nil
	}

	getClaimCount := func(id jobspb.JobID) int {
		const getClaimQuery = `SELECT count(claim_session_id) FROM system.jobs WHERE id = $1`
		count := 0
		tdb.QueryRow(t, getClaimQuery, id).Scan(&count)
		return count
	}
	// Validate the claims were not cleaned up.
	claimCount := getClaimCount(nonTerminalID)
	if claimCount == 0 {
		require.FailNowf(t, "unexpected claim sessions",
			"claim session ID's were removed some how %d", claimCount)
	}
	// Allow clean up of claimed jobs
	jobs.CancellationsUpdateLimitSetting.Override(ctx, &s.ClusterSettings().SV, 1000)
	testutils.SucceedsSoon(t, func() error {
		return checkClaimEqual(nonTerminalID, nil)
	})
	for i, id := range terminalIDs {
		require.NoError(t, checkClaimEqual(id, terminalClaims[i]))
	}
	// Update the terminal jobs to set them to have a NULL claim.
	for _, id := range terminalIDs {
		tdb.Exec(t, `UPDATE system.jobs SET claim_session_id = NULL WHERE id = $1`, id)
	}
	// At this point, all of the jobs should have a NULL claim.
	// Assert that.
	for _, id := range append(terminalIDs, nonTerminalID) {
		require.NoError(t, checkClaimEqual(id, nil))
	}

	// Nudge the adoption queue and ensure that only the non-terminal job gets
	// claimed.
	s.JobRegistry().(*jobs.Registry).TestingNudgeAdoptionQueue()

	sess, err := s.SQLLivenessProvider().(sqlliveness.Provider).Session(ctx)
	require.NoError(t, err)
	testutils.SucceedsSoon(t, func() error {
		return checkClaimEqual(nonTerminalID, sess.ID().UnsafeBytes())
	})
	// Ensure that the terminal jobs still have a nil claim.
	for _, id := range terminalIDs {
		require.NoError(t, checkClaimEqual(id, nil))
	}
}

// TestRegistrySettingUpdate checks whether the cluster settings are effective
// and properly propagated through the SQL interface. The cluster settings
// change the frequency of adopt, cancel, and gc jobs run by the registry.
func TestRegistrySettingUpdate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Default interval at the beginning of each test. The duration should be long
	// to ensure that no jobs are run in the initial phase of the tests.
	const defaultDuration = time.Hour
	// Interval to use when testing the value to go from a longer to a shorter duration.
	const shortDuration = 5 * time.Millisecond
	// Number of job runs to expect when job interval is set to shortDuration.
	const moreThan = 2
	// Base multiplier to convert defaultDuration into shortDuration
	const shortDurationBase = float64(shortDuration) / float64(defaultDuration)

	// Returns cluster settings that overrides the given setting to a long
	// defaultDuration so that the cluster setting can be tested by reducing the
	// intervals.
	clusterSettings := func(ctx context.Context, setting *settings.DurationSetting) *cluster.Settings {
		s := cluster.MakeTestingClusterSettings()
		setting.Override(ctx, &s.SV, defaultDuration)
		return s
	}

	for _, test := range [...]struct {
		name         string      // Test case ID.
		setting      string      // Cluster setting key.
		value        interface{} // Duration when expecting a large number of job runs.
		matchStmt    string      // SQL statement to match to identify the target job.
		matchAppName string
		initCount    int // Initial number of jobs to ignore at the beginning of the test.
		toOverride   *settings.DurationSetting
	}{
		{
			name:       "adopt setting",
			setting:    jobs.AdoptIntervalSettingKey,
			value:      shortDuration,
			matchStmt:  jobs.AdoptQuery,
			initCount:  0,
			toOverride: jobs.AdoptIntervalSetting,
		},
		{
			name:       "adopt setting with base",
			setting:    jobs.IntervalBaseSettingKey,
			value:      shortDurationBase,
			matchStmt:  jobs.AdoptQuery,
			initCount:  0,
			toOverride: jobs.AdoptIntervalSetting,
		},
		{
			name:       "cancel setting",
			setting:    jobs.CancelIntervalSettingKey,
			value:      shortDuration,
			matchStmt:  jobs.CancelQuery,
			initCount:  1, // 1 because a cancelLoopTask is run before the job loop.
			toOverride: jobs.CancelIntervalSetting,
		},
		{
			name:       "cancel setting with base",
			setting:    jobs.IntervalBaseSettingKey,
			value:      shortDurationBase,
			matchStmt:  jobs.CancelQuery,
			initCount:  1, // 1 because a cancelLoopTask is run before the job loop.
			toOverride: jobs.CancelIntervalSetting,
		},
		{
			name:         "gc setting",
			setting:      jobs.GcIntervalSettingKey,
			value:        shortDuration,
			matchAppName: "$ internal-gc-jobs",
			initCount:    0,
			toOverride:   jobs.GcIntervalSetting,
		},
		{
			name:         "gc setting with base",
			setting:      jobs.IntervalBaseSettingKey,
			value:        shortDurationBase,
			matchAppName: "$ internal-gc-jobs",
			initCount:    0,
			toOverride:   jobs.GcIntervalSetting,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var stmtMatcher func(*sessiondata.SessionData, string) bool
			if test.matchAppName != "" {
				stmtMatcher = func(sd *sessiondata.SessionData, _ string) bool {
					return sd.ApplicationName == test.matchAppName
				}
			} else {
				// Replace multiple white spaces with a single space, remove the last ';', and
				// trim leading and trailing spaces.
				matchStmt := strings.TrimSpace(regexp.MustCompile(`(\s+|;+)`).ReplaceAllString(test.matchStmt, " "))
				stmtMatcher = func(_ *sessiondata.SessionData, stmt string) bool {
					return stmt == matchStmt
				}
			}

			var seen = int32(0)
			stmtFilter := func(_ context.Context, sd *sessiondata.SessionData, stmt string, err error) {
				if err != nil {
					return
				}
				if stmtMatcher(sd, stmt) {
					atomic.AddInt32(&seen, 1)
				}
			}

			// Override the setting to be tested and set the value to a long duration.
			// We do so to observe rapid increase in job runs in response to updating
			// the job interval to a short duration.
			cs := clusterSettings(ctx, test.toOverride)
			args := base.TestServerArgs{
				Settings: cs,
				Knobs:    base.TestingKnobs{SQLExecutor: &sql.ExecutorTestingKnobs{StatementFilter: stmtFilter}},
			}
			s, sdb, _ := serverutils.StartServer(t, args)
			defer s.Stopper().Stop(ctx)
			tdb := sqlutils.MakeSQLRunner(sdb)

			// Wait for the initial job runs to finish.
			testutils.SucceedsSoon(t, func() error {
				counted := int(atomic.LoadInt32(&seen))
				if counted == test.initCount {
					return nil
				}
				return errors.Errorf("%s: expected at least %d calls at the beginning, counted %d",
					test.name, test.initCount, counted)
			})

			// Expect no jobs to run after a short duration to ensure that the
			// long interval times are in effect.
			atomic.StoreInt32(&seen, 0)
			time.Sleep(3 * shortDuration)
			counted := int(atomic.LoadInt32(&seen))
			require.Equalf(t, 0, counted,
				"expected no jobs after a short duration in the beginning, found %d", counted)

			// Reduce the interval and expect a larger number of job runs in a few
			// seconds.
			tdb.Exec(t, fmt.Sprintf("SET CLUSTER SETTING %s = '%v'", test.setting, test.value))
			atomic.StoreInt32(&seen, 0)
			testutils.SucceedsSoon(t, func() error {
				counted = int(atomic.LoadInt32(&seen))
				if counted >= moreThan {
					return nil
				}
				return errors.Errorf("%s: expected at least %d calls, counted %d",
					test.name, moreThan, counted)
			})
		})
	}
}

// TestGCDurationControl tests the effectiveness of job retention duration
// cluster setting and its control through the SQL interface.
func TestGCDurationControl(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer jobs.ResetConstructors()()
	ctx := context.Background()

	// Set a statement filter to monitor GC jobs that delete expired jobs.
	//
	// Replace multiple white spaces with a single space, remove the last ';', and
	// trim leading and trailing spaces.
	var seen = int32(0)
	stmtFilter := func(_ context.Context, sd *sessiondata.SessionData, _ string, err error) {
		if err != nil {
			return
		}
		if sd.ApplicationName == "$ internal-gc-jobs" {
			atomic.AddInt32(&seen, 1)
		}
	}
	cs := cluster.MakeTestingClusterSettings()
	// Ensure that GC interval and job retention duration is long in the beginning
	// of the test to ensure that the job is deleted when the retention time is
	// reduced.
	jobs.GcIntervalSetting.Override(ctx, &cs.SV, time.Hour)
	jobs.RetentionTimeSetting.Override(ctx, &cs.SV, time.Hour)
	// Shorten the adopt interval to minimize test time.
	jobs.AdoptIntervalSetting.Override(ctx, &cs.SV, 5*time.Millisecond)
	args := base.TestServerArgs{
		Settings: cs,
		Knobs: base.TestingKnobs{
			SQLExecutor: &sql.ExecutorTestingKnobs{StatementFilter: stmtFilter},
		},
	}

	defer jobs.TestingRegisterConstructor(jobspb.TypeImport, func(_ *jobs.Job, cs *cluster.Settings) jobs.Resumer {
		return jobstest.FakeResumer{}
	}, jobs.UsesTenantCostControl)()
	s, sqlDB, _ := serverutils.StartServer(t, args)
	defer s.Stopper().Stop(ctx)
	registry := s.JobRegistry().(*jobs.Registry)

	// Create and run a dummy job.
	idb := s.InternalDB().(isql.DB)
	id := registry.MakeJobID()
	require.NoError(t, idb.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		_, err := registry.CreateJobWithTxn(ctx, jobs.Record{
			// Job does not accept an empty Details field, so arbitrarily provide
			// ImportDetails.
			Details:  jobspb.ImportDetails{Walltime: 1},
			Progress: jobspb.ImportProgress{},
			Username: username.TestUserName(),
		}, id, txn)
		return err
	}))
	require.NoError(t,
		registry.WaitForJobs(
			ctx, []jobspb.JobID{id},
		))

	tdb := sqlutils.MakeSQLRunner(sqlDB)
	existsQuery := fmt.Sprintf("SELECT count(*) = 1 FROM system.jobs WHERE id = %d", id)
	infoExistsQuery := fmt.Sprintf("SELECT count(*) > 0 FROM system.job_info WHERE job_id = %d", id)
	messageExistsQuery := fmt.Sprintf("SELECT count(*) > 0 FROM system.job_message WHERE job_id = %d", id)
	statusExistsQuery := fmt.Sprintf("SELECT count(*) > 0 FROM system.job_status WHERE job_id = %d", id)
	progressExistsQuery := fmt.Sprintf("SELECT count(*) > 0 FROM system.job_progress WHERE job_id = %d", id)
	progressHistoryExistsQuery := fmt.Sprintf("SELECT count(*) > 0 FROM system.job_progress_history WHERE job_id = %d", id)

	// Make sure the job exists even though it has completed.
	tdb.CheckQueryResults(t, existsQuery, [][]string{{"true"}})
	tdb.CheckQueryResults(t, infoExistsQuery, [][]string{{"true"}})
	tdb.CheckQueryResults(t, messageExistsQuery, [][]string{{"true"}})

	// Also add some bogus rows to other job tables to test gc.
	tdb.Exec(t, fmt.Sprintf("INSERT INTO system.job_status (job_id, status) VALUES (%d, 'bogus')", id))
	tdb.Exec(t, fmt.Sprintf("INSERT INTO system.job_progress (job_id, fraction) VALUES (%d, 0.5)", id))
	tdb.Exec(t, fmt.Sprintf("INSERT INTO system.job_progress_history (job_id, fraction) VALUES (%d, 0.5)", id))

	// Shorten the GC interval to try deleting the job.
	tdb.Exec(t, fmt.Sprintf("SET CLUSTER SETTING %s = '5ms'", jobs.GcIntervalSettingKey))
	// Wait for GC to run at least once.
	atomic.StoreInt32(&seen, 0)
	testutils.SucceedsSoon(t, func() error {
		moreThan := 1
		counted := int(atomic.LoadInt32(&seen))
		if counted >= moreThan {
			return nil
		}
		return errors.Errorf("expected at least %d calls, counted %d",
			moreThan, counted)
	})
	// Make sure the job still exists.
	tdb.CheckQueryResults(t, existsQuery, [][]string{{"true"}})
	// Shorten the retention duration.
	tdb.Exec(t, fmt.Sprintf("SET CLUSTER SETTING %s = '1ms'", jobs.RetentionTimeSettingKey))
	// Wait for the job to be deleted.
	tdb.CheckQueryResultsRetry(t, existsQuery, [][]string{{"false"}})
	tdb.CheckQueryResultsRetry(t, infoExistsQuery, [][]string{{"false"}})
	tdb.CheckQueryResultsRetry(t, messageExistsQuery, [][]string{{"false"}})
	tdb.CheckQueryResultsRetry(t, statusExistsQuery, [][]string{{"false"}})
	tdb.CheckQueryResultsRetry(t, progressExistsQuery, [][]string{{"false"}})
	tdb.CheckQueryResultsRetry(t, progressHistoryExistsQuery, [][]string{{"false"}})
}

// TestWaitWithRetryableError tests retryable errors when querying
// for jobs.
func TestWaitWithRetryableError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer jobs.ResetConstructors()()
	ctx := context.Background()

	cs := cluster.MakeTestingClusterSettings()
	// Set the lease duration to zero for instanty expiry.
	lease.LeaseDuration.Override(ctx, &cs.SV, 0)
	// Renewal timeout to 0 saying that the lease will get renewed only
	// after the lease expires when a request requests the descriptor.
	lease.LeaseRenewalDuration.Override(ctx, &cs.SV, 0)

	var targetJobID atomic.Int64
	var numberOfTimesDetected atomic.Int64
	const targetNumberOfRetries = 5
	args := base.TestServerArgs{
		Settings: cs,
		// Leasing settings used above conflict with some updates
		// when starting the server, so skip those.
		PartOfCluster: true,
		Knobs: base.TestingKnobs{
			SQLExecutor: &sql.ExecutorTestingKnobs{
				DisableAutoCommitDuringExec: true,
				AfterExecute: func(ctx context.Context, stmt string, isInternal bool, err error) {
					if targetJobID.Load() > 0 &&
						strings.Contains(stmt, "SELECT count(*) FROM system.jobs") &&
						strings.Contains(stmt, fmt.Sprintf("%d", targetJobID.Load())) {
						// Leases expire almost instantly, without a renewal we will need
						// a retry.
						time.Sleep(time.Second)
						// Detect this multiple times to ensure retries, once observed
						// enough times disable the after execution.
						if numberOfTimesDetected.Add(1) > targetNumberOfRetries-1 {
							targetJobID.Store(0)
						}
					}
				},
			},
		},
	}

	defer jobs.TestingRegisterConstructor(jobspb.TypeImport, func(_ *jobs.Job, cs *cluster.Settings) jobs.Resumer {
		return jobstest.FakeResumer{}
	}, jobs.UsesTenantCostControl)()
	s := serverutils.StartServerOnly(t, args)
	defer s.Stopper().Stop(ctx)
	ts := s.ApplicationLayer()
	registry := ts.JobRegistry().(*jobs.Registry)

	// Create and run a dummy job.
	idb := ts.InternalDB().(isql.DB)
	id := registry.MakeJobID()
	require.NoError(t, idb.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		_, err := registry.CreateJobWithTxn(ctx, jobs.Record{
			// Job does not accept an empty Details field, so arbitrarily provide
			// ImportDetails.
			Details:  jobspb.ImportDetails{},
			Progress: jobspb.ImportProgress{},
			Username: username.TestUserName(),
		}, id, txn)
		return err
	}))
	targetJobID.Store(int64(id))
	require.NoError(t,
		registry.WaitForJobs(
			ctx, []jobspb.JobID{id},
		))
	if !skip.Duress() {
		require.Equalf(t, int64(targetNumberOfRetries), numberOfTimesDetected.Load(), "jobs query did not retry")
	} else {
		// For stress be lenient since we are relying on timing for leasing
		// expiration, which can be imprecise. So, lets aim for at least one
		// retry.
		require.GreaterOrEqualf(t, numberOfTimesDetected.Load(), int64(2), "jobs query did not retry")
	}
}
