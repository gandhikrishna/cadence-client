// Copyright (c) 2017-2020 Uber Technologies Inc.
// Portions of the Software are attributed to Copyright (c) 2020 Temporal Technologies Inc.
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

// This test must be its own package because workflow execution cache
// is package-level global variable, so any tests against it should belong to
// its own package to avoid inter-test interference because "go test" command
// builds one test binary per go package(even if the tests in the package are split
// among multiple .go source files) and then uses reflection on the per package
// binary to run tests.
// This means any test whose result hinges on having its own exclusive own of globals
// should be put in its own package to avoid conflicts in global variable accesses.
package evictiontest

import (
	"strconv"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
	"go.uber.org/atomic"
	"go.uber.org/cadence/.gen/go/cadence/workflowservicetest"
	m "go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/internal"
	"go.uber.org/cadence/internal/common"
	"go.uber.org/cadence/worker"
	"go.uber.org/yarpc"
	"go.uber.org/zap/zaptest"
	"golang.org/x/net/context"
)

// copied from internal/test_helpers_test.go
// this is the mock for yarpcCallOptions, as gomock requires the num of arguments to be the same.
// see getYarpcCallOptions for the default case.
func callOptions() []interface{} {
	return []interface{}{
		gomock.Any(), // library version
		gomock.Any(), // feature version
		gomock.Any(), // client name
		gomock.Any(), // feature flags
	}
}

func testReplayWorkflow(ctx internal.Context) error {
	ao := internal.ActivityOptions{
		ScheduleToStartTimeout: time.Second,
		StartToCloseTimeout:    time.Second,
	}
	ctx = internal.WithActivityOptions(ctx, ao)
	err := internal.ExecuteActivity(ctx, "testActivity").Get(ctx, nil)
	if err != nil {
		panic("Failed workflow")
	}
	return err
}

type (
	CacheEvictionSuite struct {
		suite.Suite
		mockCtrl *gomock.Controller
		service  *workflowservicetest.MockClient
	}
)

// Test suite.
func (s *CacheEvictionSuite) SetupTest() {
	s.mockCtrl = gomock.NewController(s.T())
	s.service = workflowservicetest.NewMockClient(s.mockCtrl)
}

func (s *CacheEvictionSuite) TearDownTest() {
	s.mockCtrl.Finish() // assert mock’s expectations
}

func TestWorkersTestSuite(t *testing.T) {
	suite.Run(t, new(CacheEvictionSuite))
}

func createTestEventWorkflowExecutionStarted(eventID int64, attr *m.WorkflowExecutionStartedEventAttributes) *m.HistoryEvent {
	return &m.HistoryEvent{
		EventId:                                 common.Int64Ptr(eventID),
		EventType:                               common.EventTypePtr(m.EventTypeWorkflowExecutionStarted),
		WorkflowExecutionStartedEventAttributes: attr,
	}
}

func createTestEventDecisionTaskScheduled(eventID int64, attr *m.DecisionTaskScheduledEventAttributes) *m.HistoryEvent {
	return &m.HistoryEvent{
		EventId:                              common.Int64Ptr(eventID),
		EventType:                            common.EventTypePtr(m.EventTypeDecisionTaskScheduled),
		DecisionTaskScheduledEventAttributes: attr,
	}
}

func (s *CacheEvictionSuite) TestResetStickyOnEviction() {
	testEvents := []*m.HistoryEvent{
		createTestEventWorkflowExecutionStarted(1, &m.WorkflowExecutionStartedEventAttributes{
			TaskList: &m.TaskList{Name: common.StringPtr("tasklist")},
		}),
		createTestEventDecisionTaskScheduled(2, &m.DecisionTaskScheduledEventAttributes{}),
	}

	var taskCounter atomic.Int32 // lambda variable to keep count
	// mock that manufactures unique decision tasks
	mockPollForDecisionTask := func(
		ctx context.Context,
		_PollRequest *m.PollForDecisionTaskRequest,
		opts ...yarpc.CallOption,
	) (success *m.PollForDecisionTaskResponse, err error) {
		taskID := taskCounter.Inc()
		workflowID := common.StringPtr("testID" + strconv.Itoa(int(taskID)))
		runID := common.StringPtr("runID" + strconv.Itoa(int(taskID)))
		// how we initialize the response here is the result of a series of trial and error
		// the goal is we want to fabricate a response that looks real enough to our worker
		// that it will actually go along with processing it instead of just tossing it out
		// after polling it or giving an error
		ret := &m.PollForDecisionTaskResponse{
			TaskToken:              make([]byte, 5),
			WorkflowExecution:      &m.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
			WorkflowType:           &m.WorkflowType{Name: common.StringPtr("go.uber.org/cadence/evictiontest.testReplayWorkflow")},
			History:                &m.History{Events: testEvents},
			PreviousStartedEventId: common.Int64Ptr(5)}
		return ret, nil
	}

	resetStickyAPICalled := make(chan struct{})
	mockResetStickyTaskList := func(
		ctx context.Context,
		_ResetRequest *m.ResetStickyTaskListRequest,
		opts ...yarpc.CallOption,
	) (success *m.ResetStickyTaskListResponse, err error) {
		resetStickyAPICalled <- struct{}{}
		return &m.ResetStickyTaskListResponse{}, nil
	}
	// pick 5 as cache size because it's not too big and not too small.
	cacheSize := 5
	internal.SetStickyWorkflowCacheSize(cacheSize)
	// once for workflow worker because we disable activity worker
	s.service.EXPECT().DescribeDomain(gomock.Any(), gomock.Any(), callOptions()...).Return(nil, nil).Times(1)
	// feed our worker exactly *cacheSize* "legit" decision tasks
	// these are handcrafted decision tasks that are not blatantly obviously mocks
	// the goal is to trick our worker into thinking they are real so it
	// actually goes along with processing these and puts their execution in the cache.
	s.service.EXPECT().PollForDecisionTask(gomock.Any(), gomock.Any(), callOptions()...).DoAndReturn(mockPollForDecisionTask).Times(cacheSize)
	// after *cacheSize* "legit" tasks are fed to our worker, start feeding our worker empty responses.
	// these will get tossed away immediately after polled, but we still need them so gomock doesn't compain about unexpected calls.
	// this is because our worker's poller doesn't stop, it keeps polling on the service client as long
	// as Stop() is not called on the worker
	s.service.EXPECT().PollForDecisionTask(gomock.Any(), gomock.Any(), callOptions()...).Return(&m.PollForDecisionTaskResponse{}, nil).AnyTimes()
	// this gets called after polled decision tasks are processed, any number of times doesn't matter
	s.service.EXPECT().RespondDecisionTaskCompleted(gomock.Any(), gomock.Any(), callOptions()...).Return(&m.RespondDecisionTaskCompletedResponse{}, nil).AnyTimes()
	// this is the critical point of the test.
	// ResetSticky should be called exactly once because our workflow cache evicts when full
	// so if our worker puts *cacheSize* entries in the cache, it should evict exactly one
	s.service.EXPECT().ResetStickyTaskList(gomock.Any(), gomock.Any(), callOptions()...).DoAndReturn(mockResetStickyTaskList).Times(1)

	workflowWorker := internal.NewWorker(s.service, "test-domain", "tasklist", worker.Options{
		DisableActivityWorker: true,
		Logger:                zaptest.NewLogger(s.T()),
	})
	// this is an arbitrary workflow we use for this test
	// NOTE: a simple helloworld that doesn't execute an activity
	// won't work because the workflow will simply just complete
	// and won't stay in the cache.
	// for this test, we need a workflow that "blocks" either by
	// running an activity or waiting on a timer so that its execution
	// context sticks around in the cache.
	workflowWorker.RegisterWorkflow(testReplayWorkflow)

	workflowWorker.Start()

	testTimedOut := false
	select {
	case <-time.After(time.Second * 5):
		testTimedOut = true
	case <-resetStickyAPICalled:
		// success
	}

	workflowWorker.Stop()
	s.Equal(testTimedOut, false)
}
