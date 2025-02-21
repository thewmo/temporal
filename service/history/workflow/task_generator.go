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

//go:generate mockgen -copyright_file ../../../LICENSE -package $GOPACKAGE -source $GOFILE -destination task_generator_mock.go

package workflow

import (
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"

	enumsspb "go.temporal.io/server/api/enums/v1"
	"go.temporal.io/server/common/clock"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/primitives/timestamp"
	"go.temporal.io/server/common/tasks"
)

type (
	TaskGenerator interface {
		GenerateWorkflowStartTasks(
			now time.Time,
			startEvent *historypb.HistoryEvent,
		) error
		GenerateWorkflowCloseTasks(
			now time.Time,
		) error
		GenerateRecordWorkflowStartedTasks(
			now time.Time,
			startEvent *historypb.HistoryEvent,
		) error
		GenerateDelayedWorkflowTasks(
			now time.Time,
			startEvent *historypb.HistoryEvent,
		) error
		GenerateScheduleWorkflowTaskTasks(
			now time.Time,
			workflowTaskScheduleID int64,
		) error
		GenerateStartWorkflowTaskTasks(
			now time.Time,
			workflowTaskScheduleID int64,
		) error
		GenerateActivityTransferTasks(
			now time.Time,
			event *historypb.HistoryEvent,
		) error
		GenerateActivityRetryTasks(
			activityScheduleID int64,
		) error
		GenerateChildWorkflowTasks(
			now time.Time,
			event *historypb.HistoryEvent,
		) error
		GenerateRequestCancelExternalTasks(
			now time.Time,
			event *historypb.HistoryEvent,
		) error
		GenerateSignalExternalTasks(
			now time.Time,
			event *historypb.HistoryEvent,
		) error
		GenerateWorkflowSearchAttrTasks(
			now time.Time,
		) error
		GenerateWorkflowResetTasks(
			now time.Time,
		) error

		// these 2 APIs should only be called when mutable state transaction is being closed

		GenerateActivityTimerTasks(
			now time.Time,
		) error
		GenerateUserTimerTasks(
			now time.Time,
		) error
	}

	TaskGeneratorImpl struct {
		namespaceRegistry namespace.Registry
		logger            log.Logger

		mutableState MutableState
	}
)

const defaultWorkflowRetention time.Duration = 1 * 24 * time.Hour

var _ TaskGenerator = (*TaskGeneratorImpl)(nil)

func NewTaskGenerator(
	namespaceRegistry namespace.Registry,
	logger log.Logger,
	mutableState MutableState,
) *TaskGeneratorImpl {

	mstg := &TaskGeneratorImpl{
		namespaceRegistry: namespaceRegistry,
		logger:            logger,

		mutableState: mutableState,
	}

	return mstg
}

func (r *TaskGeneratorImpl) GenerateWorkflowStartTasks(
	_ time.Time,
	startEvent *historypb.HistoryEvent,
) error {

	startVersion := startEvent.GetVersion()
	workflowRunExpirationTime := timestamp.TimeValue(r.mutableState.GetExecutionInfo().WorkflowRunExpirationTime)
	if workflowRunExpirationTime.IsZero() {
		// this mean infinite timeout
		return nil
	}

	r.mutableState.AddTimerTasks(&tasks.WorkflowTimeoutTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: workflowRunExpirationTime,
		Version:             startVersion,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateWorkflowCloseTasks(
	now time.Time,
) error {

	currentVersion := r.mutableState.GetCurrentVersion()
	executionInfo := r.mutableState.GetExecutionInfo()

	r.mutableState.AddTransferTasks(&tasks.CloseExecutionTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		Version:             currentVersion,
	})

	r.mutableState.AddVisibilityTasks(&tasks.CloseExecutionVisibilityTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		Version:             currentVersion,
	})

	retention := defaultWorkflowRetention
	namespaceEntry, err := r.namespaceRegistry.GetNamespaceByID(executionInfo.NamespaceId)
	switch err.(type) {
	case nil:
		retention = namespaceEntry.Retention(executionInfo.WorkflowId)
	case *serviceerror.NotFound:
		// namespace is not accessible, use default value above
	default:
		return err
	}

	r.mutableState.AddTimerTasks(&tasks.DeleteHistoryEventTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now.Add(retention),
		Version:             currentVersion,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateDelayedWorkflowTasks(
	now time.Time,
	startEvent *historypb.HistoryEvent,
) error {

	startVersion := startEvent.GetVersion()

	startAttr := startEvent.GetWorkflowExecutionStartedEventAttributes()
	workflowTaskBackoffDuration := timestamp.DurationValue(startAttr.GetFirstWorkflowTaskBackoff())
	executionTimestamp := now.Add(workflowTaskBackoffDuration)

	var workflowBackoffType enumsspb.WorkflowBackoffType
	switch startAttr.GetInitiator() {
	case enumspb.CONTINUE_AS_NEW_INITIATOR_RETRY:
		workflowBackoffType = enumsspb.WORKFLOW_BACKOFF_TYPE_RETRY
	case enumspb.CONTINUE_AS_NEW_INITIATOR_CRON_SCHEDULE, enumspb.CONTINUE_AS_NEW_INITIATOR_WORKFLOW:
		workflowBackoffType = enumsspb.WORKFLOW_BACKOFF_TYPE_CRON
	default:
		return serviceerror.NewInternal(fmt.Sprintf("unknown initiator: %v", startAttr.GetInitiator()))
	}

	r.mutableState.AddTimerTasks(&tasks.WorkflowBackoffTimerTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: executionTimestamp,
		WorkflowBackoffType: workflowBackoffType,
		Version:             startVersion,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateRecordWorkflowStartedTasks(
	now time.Time,
	startEvent *historypb.HistoryEvent,
) error {

	startVersion := startEvent.GetVersion()

	r.mutableState.AddVisibilityTasks(&tasks.StartExecutionVisibilityTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		Version:             startVersion,
	})
	return nil
}

func (r *TaskGeneratorImpl) GenerateScheduleWorkflowTaskTasks(
	now time.Time,
	workflowTaskScheduleID int64,
) error {

	executionInfo := r.mutableState.GetExecutionInfo()
	workflowTask, ok := r.mutableState.GetWorkflowTaskInfo(
		workflowTaskScheduleID,
	)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending workflow task: %v", workflowTaskScheduleID))
	}

	r.mutableState.AddTransferTasks(&tasks.WorkflowTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		NamespaceID:         executionInfo.NamespaceId,
		TaskQueue:           workflowTask.TaskQueue.GetName(),
		ScheduleID:          workflowTask.ScheduleID,
		Version:             workflowTask.Version,
	})

	if r.mutableState.IsStickyTaskQueueEnabled() {
		scheduledTime := timestamp.TimeValue(workflowTask.ScheduledTime)
		scheduleToStartTimeout := timestamp.DurationValue(r.mutableState.GetExecutionInfo().StickyScheduleToStartTimeout)

		r.mutableState.AddTimerTasks(&tasks.WorkflowTaskTimeoutTask{
			// TaskID is set by shard
			WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
			VisibilityTimestamp: scheduledTime.Add(scheduleToStartTimeout),
			TimeoutType:         enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START,
			EventID:             workflowTask.ScheduleID,
			ScheduleAttempt:     workflowTask.Attempt,
			Version:             workflowTask.Version,
		})
	}

	return nil
}

func (r *TaskGeneratorImpl) GenerateStartWorkflowTaskTasks(
	_ time.Time,
	workflowTaskScheduleID int64,
) error {

	workflowTask, ok := r.mutableState.GetWorkflowTaskInfo(
		workflowTaskScheduleID,
	)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending workflowTaskInfo: %v", workflowTaskScheduleID))
	}

	startedTime := timestamp.TimeValue(workflowTask.StartedTime)
	workflowTaskTimeout := timestamp.DurationValue(workflowTask.WorkflowTaskTimeout)

	r.mutableState.AddTimerTasks(&tasks.WorkflowTaskTimeoutTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: startedTime.Add(workflowTaskTimeout),
		TimeoutType:         enumspb.TIMEOUT_TYPE_START_TO_CLOSE,
		EventID:             workflowTask.ScheduleID,
		ScheduleAttempt:     workflowTask.Attempt,
		Version:             workflowTask.Version,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateActivityTransferTasks(
	now time.Time,
	event *historypb.HistoryEvent,
) error {

	activityScheduleID := event.GetEventId()
	activityInfo, ok := r.mutableState.GetActivityInfo(activityScheduleID)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending activity: %v", activityScheduleID))
	}

	r.mutableState.AddTransferTasks(&tasks.ActivityTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		TargetNamespaceID:   activityInfo.NamespaceId,
		TaskQueue:           activityInfo.TaskQueue,
		ScheduleID:          activityInfo.ScheduleId,
		Version:             activityInfo.Version,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateActivityRetryTasks(
	activityScheduleID int64,
) error {

	ai, ok := r.mutableState.GetActivityInfo(activityScheduleID)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending activity: %v", activityScheduleID))
	}

	r.mutableState.AddTimerTasks(&tasks.ActivityRetryTimerTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		Version:             ai.Version,
		VisibilityTimestamp: *ai.ScheduledTime,
		EventID:             ai.ScheduleId,
		Attempt:             ai.Attempt,
	})
	return nil
}

func (r *TaskGeneratorImpl) GenerateChildWorkflowTasks(
	now time.Time,
	event *historypb.HistoryEvent,
) error {

	attr := event.GetStartChildWorkflowExecutionInitiatedEventAttributes()
	childWorkflowScheduleID := event.GetEventId()
	childWorkflowTargetNamespace := attr.GetNamespace()

	childWorkflowInfo, ok := r.mutableState.GetChildExecutionInfo(childWorkflowScheduleID)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending child workflow: %v", childWorkflowScheduleID))
	}

	targetNamespaceID, err := r.getTargetNamespaceID(childWorkflowTargetNamespace)
	if err != nil {
		return err
	}

	r.mutableState.AddTransferTasks(&tasks.StartChildExecutionTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		TargetNamespaceID:   targetNamespaceID,
		TargetWorkflowID:    childWorkflowInfo.StartedWorkflowId,
		InitiatedID:         childWorkflowInfo.InitiatedId,
		Version:             childWorkflowInfo.Version,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateRequestCancelExternalTasks(
	now time.Time,
	event *historypb.HistoryEvent,
) error {

	attr := event.GetRequestCancelExternalWorkflowExecutionInitiatedEventAttributes()
	scheduleID := event.GetEventId()
	version := event.GetVersion()
	targetNamespace := attr.GetNamespace()
	targetWorkflowID := attr.GetWorkflowExecution().GetWorkflowId()
	targetRunID := attr.GetWorkflowExecution().GetRunId()
	targetChildOnly := attr.GetChildWorkflowOnly()

	_, ok := r.mutableState.GetRequestCancelInfo(scheduleID)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending request cancel external workflow: %v", scheduleID))
	}

	targetNamespaceID, err := r.getTargetNamespaceID(targetNamespace)
	if err != nil {
		return err
	}

	r.mutableState.AddTransferTasks(&tasks.CancelExecutionTask{
		// TaskID is set by shard
		WorkflowIdentifier:      r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp:     now,
		TargetNamespaceID:       targetNamespaceID,
		TargetWorkflowID:        targetWorkflowID,
		TargetRunID:             targetRunID,
		TargetChildWorkflowOnly: targetChildOnly,
		InitiatedID:             scheduleID,
		Version:                 version,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateSignalExternalTasks(
	now time.Time,
	event *historypb.HistoryEvent,
) error {

	attr := event.GetSignalExternalWorkflowExecutionInitiatedEventAttributes()
	scheduleID := event.GetEventId()
	version := event.GetVersion()
	targetNamespace := attr.GetNamespace()
	targetWorkflowID := attr.GetWorkflowExecution().GetWorkflowId()
	targetRunID := attr.GetWorkflowExecution().GetRunId()
	targetChildOnly := attr.GetChildWorkflowOnly()

	_, ok := r.mutableState.GetSignalInfo(scheduleID)
	if !ok {
		return serviceerror.NewInternal(fmt.Sprintf("it could be a bug, cannot get pending signal external workflow: %v", scheduleID))
	}

	targetNamespaceID, err := r.getTargetNamespaceID(targetNamespace)
	if err != nil {
		return err
	}

	r.mutableState.AddTransferTasks(&tasks.SignalExecutionTask{
		// TaskID is set by shard
		WorkflowIdentifier:      r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp:     now,
		TargetNamespaceID:       targetNamespaceID,
		TargetWorkflowID:        targetWorkflowID,
		TargetRunID:             targetRunID,
		TargetChildWorkflowOnly: targetChildOnly,
		InitiatedID:             scheduleID,
		Version:                 version,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateWorkflowSearchAttrTasks(
	now time.Time,
) error {

	currentVersion := r.mutableState.GetCurrentVersion()

	r.mutableState.AddVisibilityTasks(&tasks.UpsertExecutionVisibilityTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		Version:             currentVersion, // task processing does not check this version
	})
	return nil
}

func (r *TaskGeneratorImpl) GenerateWorkflowResetTasks(
	now time.Time,
) error {

	currentVersion := r.mutableState.GetCurrentVersion()

	r.mutableState.AddTransferTasks(&tasks.ResetWorkflowTask{
		// TaskID is set by shard
		WorkflowIdentifier:  r.mutableState.GetWorkflowIdentifier(),
		VisibilityTimestamp: now,
		Version:             currentVersion,
	})

	return nil
}

func (r *TaskGeneratorImpl) GenerateActivityTimerTasks(
	now time.Time,
) error {

	_, err := r.getTimerSequence(now).CreateNextActivityTimer()
	return err
}

func (r *TaskGeneratorImpl) GenerateUserTimerTasks(
	now time.Time,
) error {

	_, err := r.getTimerSequence(now).CreateNextUserTimer()
	return err
}

func (r *TaskGeneratorImpl) getTimerSequence(now time.Time) TimerSequence {
	timeSource := clock.NewEventTimeSource()
	timeSource.Update(now)
	return NewTimerSequence(timeSource, r.mutableState)
}

func (r *TaskGeneratorImpl) getTargetNamespaceID(
	targetNamespace string,
) (string, error) {

	targetNamespaceID := r.mutableState.GetExecutionInfo().NamespaceId
	if targetNamespace != "" {
		targetNamespaceEntry, err := r.namespaceRegistry.GetNamespace(targetNamespace)
		if err != nil {
			return "", err
		}
		targetNamespaceID = targetNamespaceEntry.ID()
	}

	return targetNamespaceID, nil
}
