// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/distsql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/stretchr/testify/require"
)

// Test that we don't attempt to create flows in an aborted transaction.
// Instead, a retryable error is created on the gateway. The point is to
// simulate a race where the heartbeat loop finds out that the txn is aborted
// just before a plan starts execution and check that we don't create flows in
// an aborted txn (which isn't allowed). Note that, once running, each flow can
// discover on its own that its txn is aborted - that's handled separately. But
// flows can't start in a txn that's already known to be aborted.
//
// We test this by manually aborting a txn and then attempting to execute a plan
// in it. We're careful to not use the transaction for anything but running the
// plan; planning will be performed outside of the transaction.
func TestDistSQLRunningInAbortedTxn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, sqlDB, db := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	if _, err := sqlDB.ExecContext(
		ctx, "create database test; create table test.t(a int)"); err != nil {
		t.Fatal(err)
	}
	key := roachpb.Key("a")

	// Plan a statement.
	execCfg := s.ExecutorConfig().(ExecutorConfig)
	internalPlanner, cleanup := NewInternalPlanner(
		"test",
		kv.NewTxn(ctx, db, s.NodeID()),
		security.RootUserName(),
		&MemoryMetrics{},
		&execCfg,
		sessiondatapb.SessionData{},
	)
	defer cleanup()
	p := internalPlanner.(*planner)
	query := "select * from test.t"
	stmt, err := parser.ParseOne(query)
	if err != nil {
		t.Fatal(err)
	}

	push := func(ctx context.Context, key roachpb.Key) error {
		// Conflicting transaction that pushes another transaction.
		conflictTxn := kv.NewTxn(ctx, db, 0 /* gatewayNodeID */)
		// We need to explicitly set a high priority for the push to happen.
		if err := conflictTxn.SetUserPriority(roachpb.MaxUserPriority); err != nil {
			return err
		}
		// Push through a Put, as opposed to a Get, so that the pushee gets aborted.
		if err := conflictTxn.Put(ctx, key, "pusher was here"); err != nil {
			return err
		}
		return conflictTxn.CommitOrCleanup(ctx)
	}

	// Make a db with a short heartbeat interval, so that the aborted txn finds
	// out quickly.
	ambient := log.AmbientContext{Tracer: tracing.NewTracer()}
	tsf := kvcoord.NewTxnCoordSenderFactory(
		kvcoord.TxnCoordSenderFactoryConfig{
			AmbientCtx: ambient,
			// Short heartbeat interval.
			HeartbeatInterval: time.Millisecond,
			Settings:          s.ClusterSettings(),
			Clock:             s.Clock(),
			Stopper:           s.Stopper(),
		},
		s.DistSenderI().(*kvcoord.DistSender),
	)
	shortDB := kv.NewDB(ambient, tsf, s.Clock(), s.Stopper())

	iter := 0
	// We'll trace to make sure the test isn't fooling itself.
	runningCtx, getRec, cancel := tracing.ContextWithRecordingSpan(ctx, "test")
	defer cancel()
	err = shortDB.Txn(runningCtx, func(ctx context.Context, txn *kv.Txn) error {
		iter++
		if iter == 1 {
			// On the first iteration, abort the txn.

			if err := txn.Put(ctx, key, "val"); err != nil {
				t.Fatal(err)
			}

			if err := push(ctx, key); err != nil {
				t.Fatal(err)
			}

			// Now wait until the heartbeat loop notices that the transaction is aborted.
			testutils.SucceedsSoon(t, func() error {
				if txn.Sender().(*kvcoord.TxnCoordSender).IsTracking() {
					return fmt.Errorf("txn heartbeat loop running")
				}
				return nil
			})
		}

		// Create and run a DistSQL plan.
		rw := newCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
			return nil
		})
		recv := MakeDistSQLReceiver(
			ctx,
			rw,
			stmt.AST.StatementType(),
			execCfg.RangeDescriptorCache,
			txn,
			execCfg.Clock,
			p.ExtendedEvalContext().Tracing,
			execCfg.ContentionRegistry,
		)

		// We need to re-plan every time, since close() below makes
		// the plan unusable across retries.
		p.stmt = makeStatement(stmt, ClusterWideID{})
		if err := p.makeOptimizerPlan(ctx); err != nil {
			t.Fatal(err)
		}
		defer p.curPlan.close(ctx)

		evalCtx := p.ExtendedEvalContext()
		// We need distribute = true so that executing the plan involves marshaling
		// the root txn meta to leaf txns. Local flows can start in aborted txns
		// because they just use the root txn.
		planCtx := execCfg.DistSQLPlanner.NewPlanningCtx(ctx, evalCtx, p, nil /* txn */, true /* distribute */)
		planCtx.stmtType = recv.stmtType

		execCfg.DistSQLPlanner.PlanAndRun(
			ctx, evalCtx, planCtx, txn, p.curPlan.main, recv,
		)()
		return rw.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if iter != 2 {
		t.Fatalf("expected two iterations, but txn took %d to succeed", iter)
	}
	if tracing.FindMsgInRecording(getRec(), clientRejectedMsg) == -1 {
		t.Fatalf("didn't find expected message in trace: %s", clientRejectedMsg)
	}
}

// Test that the DistSQLReceiver overwrites previous errors as "better" errors
// come along.
func TestDistSQLReceiverErrorRanking(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This test goes through the trouble of creating a server because it wants to
	// create a txn. It creates the txn because it wants to test an interaction
	// between the DistSQLReceiver and the TxnCoordSender: the DistSQLReceiver
	// will feed retriable errors to the TxnCoordSender which will change those
	// errors to TransactionRetryWithProtoRefreshError.
	ctx := context.Background()
	s, _, db := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	txn := kv.NewTxn(ctx, db, s.NodeID())

	// We're going to use a rowResultWriter to which only errors will be passed.
	rw := newCallbackResultWriter(nil /* fn */)
	recv := MakeDistSQLReceiver(
		ctx,
		rw,
		tree.Rows, /* StatementType */
		nil,       /* rangeCache */
		txn,
		nil, /* clockUpdater */
		&SessionTracing{},
		nil, /* contentionRegistry */
	)

	retryErr := roachpb.NewErrorWithTxn(
		roachpb.NewTransactionRetryError(
			roachpb.RETRY_SERIALIZABLE, "test err"),
		txn.TestingCloneTxn()).GoError()

	abortErr := roachpb.NewErrorWithTxn(
		roachpb.NewTransactionAbortedError(
			roachpb.ABORT_REASON_ABORTED_RECORD_FOUND),
		txn.TestingCloneTxn()).GoError()

	errs := []struct {
		err    error
		expErr string
	}{
		{
			// Initial error, retriable.
			err:    retryErr,
			expErr: "TransactionRetryWithProtoRefreshError: TransactionRetryError",
		},
		{
			// A non-retriable error overwrites a retriable one.
			err:    fmt.Errorf("err1"),
			expErr: "err1",
		},
		{
			// Another non-retriable error doesn't overwrite the previous one.
			err:    fmt.Errorf("err2"),
			expErr: "err1",
		},
		{
			// A TransactionAbortedError overwrites anything.
			err:    abortErr,
			expErr: "TransactionRetryWithProtoRefreshError: TransactionAbortedError",
		},
		{
			// A non-aborted retriable error does not overried the
			// TransactionAbortedError.
			err:    retryErr,
			expErr: "TransactionRetryWithProtoRefreshError: TransactionAbortedError",
		},
	}

	for i, tc := range errs {
		recv.Push(nil, /* row */
			&execinfrapb.ProducerMetadata{
				Err: tc.err,
			})
		if !testutils.IsError(rw.Err(), tc.expErr) {
			t.Fatalf("%d: expected %s, got %s", i, tc.expErr, rw.Err())
		}
	}
}

// TestDistSQLReceiverReportsContention verifies that the distsql receiver
// reports contentions event via an observable metric if they occur. This test
// additionally verifies that the metric stays at zero if there is no
// contention.
func TestDistSQLReceiverReportsContention(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	testutils.RunTrueAndFalse(t, "contention", func(t *testing.T, contention bool) {
		s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
			Knobs: base.TestingKnobs{
				DistSQL: &execinfra.TestingKnobs{
					GenerateMockContentionEvents: contention,
				},
			},
		})
		defer s.Stopper().Stop(ctx)

		sqlutils.CreateTable(
			t, db, "test", "x INT", 10, sqlutils.ToRowFn(sqlutils.RowIdxFn),
		)

		metrics := s.DistSQLServer().(*distsql.ServerImpl).Metrics
		for _, query := range []string{
			"SELECT * FROM test.test",
			// TODO(asubiotto): Uncomment once contention metadata is propagated back
			//  from planNodes (#56916).
			// "INSERT INTO test.test VALUES (11)",
		} {
			metrics.ContendedQueriesCount.Clear()
			_, err := db.ExecContext(ctx, query)
			require.NoError(t, err)

			if contention {
				// Soft check to protect against flakiness where an internal query
				// causes the contention metric to increment.
				require.GreaterOrEqual(t, metrics.ContendedQueriesCount.Count(), int64(1))
			} else {
				require.Zero(
					t,
					metrics.ContendedQueriesCount.Count(),
					"contention metric unexpectedly non-zero when no contention events are produced",
				)
			}
		}
	})
}
