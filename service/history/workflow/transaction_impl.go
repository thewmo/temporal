// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package workflow

import (
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"

	"go.temporal.io/server/common"
	"go.temporal.io/server/common/backoff"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/versionhistory"
	"go.temporal.io/server/common/tasks"
	"go.temporal.io/server/service/history/consts"
	"go.temporal.io/server/service/history/events"
	"go.temporal.io/server/service/history/shard"
)

type (
	TransactionImpl struct {
		shard  shard.Context
		logger log.Logger
	}
)

var _ Transaction = (*TransactionImpl)(nil)

func NewTransaction(
	shard shard.Context,
) *TransactionImpl {
	return &TransactionImpl{
		shard:  shard,
		logger: shard.GetLogger(),
	}
}

func (t *TransactionImpl) CreateWorkflowExecution(
	createMode persistence.CreateWorkflowMode,
	newWorkflowSnapshot *persistence.WorkflowSnapshot,
	newWorkflowEventsSeq []*persistence.WorkflowEvents,
) (int64, error) {

	resp, err := createWorkflowExecutionWithRetry(t.shard, &persistence.CreateWorkflowExecutionRequest{
		ShardID: t.shard.GetShardID(),
		// RangeID , this is set by shard context
		Mode:                createMode,
		NewWorkflowSnapshot: *newWorkflowSnapshot,
		NewWorkflowEvents:   newWorkflowEventsSeq,
	})
	if err != nil {
		return 0, err
	}

	engine := t.shard.GetEngine()
	NotifyWorkflowSnapshotTasks(engine, newWorkflowSnapshot)
	if err := NotifyNewHistorySnapshotEvent(engine, newWorkflowSnapshot); err != nil {
		t.logger.Error("unable to notify workflow creation", tag.Error(err))
	}

	return int64(resp.NewMutableStateStats.HistoryStatistics.SizeDiff), nil
}

func (t *TransactionImpl) ConflictResolveWorkflowExecution(
	conflictResolveMode persistence.ConflictResolveWorkflowMode,
	resetWorkflowSnapshot *persistence.WorkflowSnapshot,
	resetWorkflowEventsSeq []*persistence.WorkflowEvents,
	newWorkflowSnapshot *persistence.WorkflowSnapshot,
	newWorkflowEventsSeq []*persistence.WorkflowEvents,
	currentWorkflowMutation *persistence.WorkflowMutation,
	currentWorkflowEventsSeq []*persistence.WorkflowEvents,
) (int64, int64, int64, error) {

	resp, err := conflictResolveWorkflowExecutionWithRetry(t.shard, &persistence.ConflictResolveWorkflowExecutionRequest{
		ShardID: t.shard.GetShardID(),
		// RangeID , this is set by shard context
		Mode:                    conflictResolveMode,
		ResetWorkflowSnapshot:   *resetWorkflowSnapshot,
		ResetWorkflowEvents:     resetWorkflowEventsSeq,
		NewWorkflowSnapshot:     newWorkflowSnapshot,
		NewWorkflowEvents:       newWorkflowEventsSeq,
		CurrentWorkflowMutation: currentWorkflowMutation,
		CurrentWorkflowEvents:   currentWorkflowEventsSeq,
	})
	if err != nil {
		return 0, 0, 0, err
	}

	engine := t.shard.GetEngine()
	NotifyWorkflowSnapshotTasks(engine, resetWorkflowSnapshot)
	NotifyWorkflowSnapshotTasks(engine, newWorkflowSnapshot)
	NotifyWorkflowMutationTasks(engine, currentWorkflowMutation)
	if err := NotifyNewHistorySnapshotEvent(engine, resetWorkflowSnapshot); err != nil {
		t.logger.Error("unable to notify workflow reset", tag.Error(err))
	}
	if err := NotifyNewHistorySnapshotEvent(engine, newWorkflowSnapshot); err != nil {
		t.logger.Error("unable to notify workflow creation", tag.Error(err))
	}
	if err := NotifyNewHistoryMutationEvent(engine, currentWorkflowMutation); err != nil {
		t.logger.Error("unable to notify workflow mutation", tag.Error(err))
	}
	resetHistorySizeDiff := int64(resp.ResetMutableStateStats.HistoryStatistics.SizeDiff)
	newHistorySizeDiff := int64(0)
	if resp.NewMutableStateStats != nil {
		newHistorySizeDiff = int64(resp.NewMutableStateStats.HistoryStatistics.SizeDiff)
	}
	currentHistorySizeDiff := int64(0)
	if resp.CurrentMutableStateStats != nil {
		currentHistorySizeDiff = int64(resp.CurrentMutableStateStats.HistoryStatistics.SizeDiff)
	}
	return resetHistorySizeDiff, newHistorySizeDiff, currentHistorySizeDiff, nil
}

func (t *TransactionImpl) UpdateWorkflowExecution(
	updateMode persistence.UpdateWorkflowMode,
	currentWorkflowMutation *persistence.WorkflowMutation,
	currentWorkflowEventsSeq []*persistence.WorkflowEvents,
	newWorkflowSnapshot *persistence.WorkflowSnapshot,
	newWorkflowEventsSeq []*persistence.WorkflowEvents,
) (int64, int64, error) {

	resp, err := updateWorkflowExecutionWithRetry(t.shard, &persistence.UpdateWorkflowExecutionRequest{
		ShardID: t.shard.GetShardID(),
		// RangeID , this is set by shard context
		Mode:                   updateMode,
		UpdateWorkflowMutation: *currentWorkflowMutation,
		UpdateWorkflowEvents:   currentWorkflowEventsSeq,
		NewWorkflowSnapshot:    newWorkflowSnapshot,
		NewWorkflowEvents:      newWorkflowEventsSeq,
	})
	if err != nil {
		return 0, 0, err
	}

	engine := t.shard.GetEngine()
	NotifyWorkflowMutationTasks(engine, currentWorkflowMutation)
	NotifyWorkflowSnapshotTasks(engine, newWorkflowSnapshot)
	if err := NotifyNewHistoryMutationEvent(engine, currentWorkflowMutation); err != nil {
		t.logger.Error("unable to notify workflow mutation", tag.Error(err))
	}
	if err := NotifyNewHistorySnapshotEvent(engine, newWorkflowSnapshot); err != nil {
		t.logger.Error("unable to notify workflow creation", tag.Error(err))
	}
	updateHistorySizeDiff := int64(resp.UpdateMutableStateStats.HistoryStatistics.SizeDiff)
	newHistorySizeDiff := int64(0)
	if resp.NewMutableStateStats != nil {
		newHistorySizeDiff = int64(resp.NewMutableStateStats.HistoryStatistics.SizeDiff)
	}
	return updateHistorySizeDiff, newHistorySizeDiff, nil
}

func PersistWorkflowEvents(
	shard shard.Context,
	workflowEvents *persistence.WorkflowEvents,
) (int64, error) {

	if len(workflowEvents.Events) == 0 {
		return 0, nil // allow update workflow without events
	}

	firstEventID := workflowEvents.Events[0].EventId
	if firstEventID == common.FirstEventID {
		return persistFirstWorkflowEvents(shard, workflowEvents)
	}
	return persistNonFirstWorkflowEvents(shard, workflowEvents)
}

func persistFirstWorkflowEvents(
	shard shard.Context,
	workflowEvents *persistence.WorkflowEvents,
) (int64, error) {

	namespaceID := workflowEvents.NamespaceID
	workflowID := workflowEvents.WorkflowID
	runID := workflowEvents.RunID
	execution := commonpb.WorkflowExecution{
		WorkflowId: workflowEvents.WorkflowID,
		RunId:      workflowEvents.RunID,
	}
	branchToken := workflowEvents.BranchToken
	events := workflowEvents.Events
	prevTxnID := workflowEvents.PrevTxnID
	txnID := workflowEvents.TxnID

	size, err := appendHistoryV2EventsWithRetry(
		shard,
		namespaceID,
		execution,
		&persistence.AppendHistoryNodesRequest{
			IsNewBranch:       true,
			Info:              persistence.BuildHistoryGarbageCleanupInfo(namespaceID, workflowID, runID),
			BranchToken:       branchToken,
			Events:            events,
			PrevTransactionID: prevTxnID,
			TransactionID:     txnID,
		},
	)
	return size, err
}

func persistNonFirstWorkflowEvents(
	shard shard.Context,
	workflowEvents *persistence.WorkflowEvents,
) (int64, error) {

	if len(workflowEvents.Events) == 0 {
		return 0, nil // allow update workflow without events
	}

	namespaceID := workflowEvents.NamespaceID
	execution := commonpb.WorkflowExecution{
		WorkflowId: workflowEvents.WorkflowID,
		RunId:      workflowEvents.RunID,
	}
	branchToken := workflowEvents.BranchToken
	events := workflowEvents.Events
	prevTxnID := workflowEvents.PrevTxnID
	txnID := workflowEvents.TxnID

	size, err := appendHistoryV2EventsWithRetry(
		shard,
		namespaceID,
		execution,
		&persistence.AppendHistoryNodesRequest{
			IsNewBranch:       false,
			BranchToken:       branchToken,
			Events:            events,
			PrevTransactionID: prevTxnID,
			TransactionID:     txnID,
		},
	)
	return size, err
}

func appendHistoryV2EventsWithRetry(
	shard shard.Context,
	namespaceID string,
	execution commonpb.WorkflowExecution,
	request *persistence.AppendHistoryNodesRequest,
) (int64, error) {

	resp := 0
	op := func() error {
		var err error
		resp, err = shard.AppendHistoryEvents(request, namespaceID, execution)
		return err
	}

	err := backoff.Retry(
		op,
		PersistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	return int64(resp), err
}

func createWorkflowExecutionWithRetry(
	shard shard.Context,
	request *persistence.CreateWorkflowExecutionRequest,
) (*persistence.CreateWorkflowExecutionResponse, error) {

	var resp *persistence.CreateWorkflowExecutionResponse
	op := func() error {
		var err error
		resp, err = shard.CreateWorkflowExecution(request)
		return err
	}

	err := backoff.Retry(
		op,
		PersistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		emitMutationMetrics(
			shard,
			request.NewWorkflowSnapshot.ExecutionInfo.NamespaceId,
			[]*persistence.MutableStateStatistics{
				&resp.NewMutableStateStats,
			},
		)
		return resp, nil
	case *persistence.CurrentWorkflowConditionFailedError,
		*persistence.WorkflowConditionFailedError,
		*persistence.ConditionFailedError:
		// it is possible that workflow already exists and caller need to apply
		// workflow ID reuse policy
		return nil, err
	default:
		shard.GetLogger().Error(
			"Persistent store operation Failure",
			tag.WorkflowNamespaceID(request.NewWorkflowSnapshot.ExecutionInfo.NamespaceId),
			tag.WorkflowID(request.NewWorkflowSnapshot.ExecutionInfo.WorkflowId),
			tag.WorkflowRunID(request.NewWorkflowSnapshot.ExecutionState.RunId),
			tag.StoreOperationCreateWorkflowExecution,
			tag.Error(err),
		)
		return nil, err
	}
}

func conflictResolveWorkflowExecutionWithRetry(
	shard shard.Context,
	request *persistence.ConflictResolveWorkflowExecutionRequest,
) (*persistence.ConflictResolveWorkflowExecutionResponse, error) {

	var resp *persistence.ConflictResolveWorkflowExecutionResponse
	op := func() error {
		var err error
		resp, err = shard.ConflictResolveWorkflowExecution(request)
		return err
	}

	err := backoff.Retry(
		op,
		PersistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		emitMutationMetrics(
			shard,
			request.ResetWorkflowSnapshot.ExecutionInfo.NamespaceId,
			[]*persistence.MutableStateStatistics{
				&resp.ResetMutableStateStats,
				resp.NewMutableStateStats,
				resp.CurrentMutableStateStats,
			},
		)
		return resp, nil
	case *persistence.CurrentWorkflowConditionFailedError,
		*persistence.WorkflowConditionFailedError,
		*persistence.ConditionFailedError:
		// it is possible that workflow already exists and caller need to apply
		// workflow ID reuse policy
		return nil, err
	default:
		shard.GetLogger().Error(
			"Persistent store operation Failure",
			tag.WorkflowNamespaceID(request.ResetWorkflowSnapshot.ExecutionInfo.NamespaceId),
			tag.WorkflowID(request.ResetWorkflowSnapshot.ExecutionInfo.WorkflowId),
			tag.WorkflowRunID(request.ResetWorkflowSnapshot.ExecutionState.RunId),
			tag.StoreOperationConflictResolveWorkflowExecution,
			tag.Error(err),
		)
		return nil, err
	}
}

func getWorkflowExecutionWithRetry(
	shard shard.Context,
	request *persistence.GetWorkflowExecutionRequest,
) (*persistence.GetWorkflowExecutionResponse, error) {

	var resp *persistence.GetWorkflowExecutionResponse
	op := func() error {
		var err error
		resp, err = shard.GetExecutionManager().GetWorkflowExecution(request)

		return err
	}

	err := backoff.Retry(
		op,
		PersistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		emitGetMetrics(
			shard,
			resp.State.ExecutionInfo.NamespaceId,
			[]*persistence.MutableStateStatistics{
				&resp.MutableStateStats,
			},
		)
		return resp, nil
	case *serviceerror.NotFound:
		// it is possible that workflow does not exists
		return nil, err
	default:
		shard.GetLogger().Error(
			"Persistent fetch operation Failure",
			tag.WorkflowNamespaceID(request.NamespaceID),
			tag.WorkflowID(request.Execution.WorkflowId),
			tag.WorkflowRunID(request.Execution.RunId),
			tag.StoreOperationGetWorkflowExecution,
			tag.Error(err),
		)
		return nil, err
	}
}

func updateWorkflowExecutionWithRetry(
	shard shard.Context,
	request *persistence.UpdateWorkflowExecutionRequest,
) (*persistence.UpdateWorkflowExecutionResponse, error) {

	var resp *persistence.UpdateWorkflowExecutionResponse
	var err error
	op := func() error {
		resp, err = shard.UpdateWorkflowExecution(request)
		return err
	}

	err = backoff.Retry(
		op, PersistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		emitMutationMetrics(
			shard,
			request.UpdateWorkflowMutation.ExecutionInfo.NamespaceId,
			[]*persistence.MutableStateStatistics{
				&resp.UpdateMutableStateStats,
				resp.NewMutableStateStats,
			},
		)
		return resp, nil
	case *persistence.CurrentWorkflowConditionFailedError,
		*persistence.WorkflowConditionFailedError,
		*persistence.ConditionFailedError:
		// TODO get rid of ErrConflict
		return nil, consts.ErrConflict
	default:
		shard.GetLogger().Error(
			"Persistent store operation Failure",
			tag.WorkflowNamespaceID(request.UpdateWorkflowMutation.ExecutionInfo.NamespaceId),
			tag.WorkflowID(request.UpdateWorkflowMutation.ExecutionInfo.WorkflowId),
			tag.WorkflowRunID(request.UpdateWorkflowMutation.ExecutionState.RunId),
			tag.StoreOperationUpdateWorkflowExecution,
			tag.Error(err),
		)
		return nil, err
	}
}

func NotifyWorkflowSnapshotTasks(
	engine shard.Engine,
	workflowSnapshot *persistence.WorkflowSnapshot,
) {
	if workflowSnapshot == nil {
		return
	}
	notifyTasks(
		engine,
		workflowSnapshot.TransferTasks,
		workflowSnapshot.TimerTasks,
		workflowSnapshot.ReplicationTasks,
		workflowSnapshot.VisibilityTasks,
	)
}

func NotifyWorkflowMutationTasks(
	engine shard.Engine,
	workflowMutation *persistence.WorkflowMutation,
) {
	if workflowMutation == nil {
		return
	}
	notifyTasks(
		engine,
		workflowMutation.TransferTasks,
		workflowMutation.TimerTasks,
		workflowMutation.ReplicationTasks,
		workflowMutation.VisibilityTasks,
	)
}

func notifyTasks(
	engine shard.Engine,
	transferTasks []tasks.Task,
	timerTasks []tasks.Task,
	replicationTasks []tasks.Task,
	visibilityTasks []tasks.Task,
) {
	engine.NotifyNewTransferTasks(transferTasks)
	engine.NotifyNewTimerTasks(timerTasks)
	engine.NotifyNewVisibilityTasks(visibilityTasks)
	engine.NotifyNewReplicationTasks(replicationTasks)
}

func NotifyNewHistorySnapshotEvent(
	engine shard.Engine,
	workflowSnapshot *persistence.WorkflowSnapshot,
) error {

	if workflowSnapshot == nil {
		return nil
	}

	executionInfo := workflowSnapshot.ExecutionInfo
	executionState := workflowSnapshot.ExecutionState

	namespaceID := executionInfo.NamespaceId
	workflowID := executionInfo.WorkflowId
	runID := executionState.RunId
	currentVersionHistory, err := versionhistory.GetCurrentVersionHistory(executionInfo.VersionHistories)
	if err != nil {
		return err
	}
	currentBranchToken := currentVersionHistory.BranchToken
	workflowState := executionState.State
	workflowStatus := executionState.Status
	lastFirstEventID := executionInfo.LastFirstEventId
	lastFirstEventTxnID := executionInfo.LastFirstEventTxnId
	lastWorkflowTaskStartEventID := executionInfo.LastWorkflowTaskStartId
	nextEventID := workflowSnapshot.NextEventID

	engine.NotifyNewHistoryEvent(events.NewNotification(
		namespaceID,
		&commonpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
		lastFirstEventID,
		lastFirstEventTxnID,
		nextEventID,
		lastWorkflowTaskStartEventID,
		currentBranchToken,
		workflowState,
		workflowStatus,
	))
	return nil
}

func NotifyNewHistoryMutationEvent(
	engine shard.Engine,
	workflowMutation *persistence.WorkflowMutation,
) error {

	if workflowMutation == nil {
		return nil
	}

	executionInfo := workflowMutation.ExecutionInfo
	executionState := workflowMutation.ExecutionState

	namespaceID := executionInfo.NamespaceId
	workflowID := executionInfo.WorkflowId
	runID := executionState.RunId
	currentVersionHistory, err := versionhistory.GetCurrentVersionHistory(executionInfo.VersionHistories)
	if err != nil {
		return err
	}
	currentBranchToken := currentVersionHistory.BranchToken
	workflowState := executionState.State
	workflowStatus := executionState.Status
	lastFirstEventID := executionInfo.LastFirstEventId
	lastFirstEventTxnID := executionInfo.LastFirstEventTxnId
	lastWorkflowTaskStartEventID := executionInfo.LastWorkflowTaskStartId
	nextEventID := workflowMutation.NextEventID

	engine.NotifyNewHistoryEvent(events.NewNotification(
		namespaceID,
		&commonpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
		lastFirstEventID,
		lastFirstEventTxnID,
		nextEventID,
		lastWorkflowTaskStartEventID,
		currentBranchToken,
		workflowState,
		workflowStatus,
	))
	return nil
}

func emitMutationMetrics(
	shard shard.Context,
	namespaceID string,
	stats []*persistence.MutableStateStatistics,
) {
	namespaceEntry, err := shard.GetNamespaceRegistry().GetNamespaceByID(
		namespaceID,
	)
	if err != nil {
		return
	}

	metricsClient := shard.GetMetricsClient()
	namespaceName := namespaceEntry.Name()
	for _, stat := range stats {
		emitMutableStateStatus(
			metricsClient.Scope(metrics.SessionSizeStatsScope, metrics.NamespaceTag(namespaceName)),
			metricsClient.Scope(metrics.SessionCountStatsScope, metrics.NamespaceTag(namespaceName)),
			stat,
		)
	}
}

func emitGetMetrics(
	shard shard.Context,
	namespaceID string,
	stats []*persistence.MutableStateStatistics,
) {
	namespaceEntry, err := shard.GetNamespaceRegistry().GetNamespaceByID(
		namespaceID,
	)
	if err != nil {
		return
	}

	metricsClient := shard.GetMetricsClient()
	namespaceName := namespaceEntry.Name()
	for _, stat := range stats {
		emitMutableStateStatus(
			metricsClient.Scope(metrics.ExecutionSizeStatsScope, metrics.NamespaceTag(namespaceName)),
			metricsClient.Scope(metrics.ExecutionCountStatsScope, metrics.NamespaceTag(namespaceName)),
			stat,
		)
	}
}
