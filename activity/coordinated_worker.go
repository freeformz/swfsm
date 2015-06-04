package activity

import (
	"log"
	"time"

	"strings"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/swf"
	. "github.com/sclasen/swfsm/sugar"
)

const (
	TaskGone = "Unknown activity"
)

// AddCoordinatedHandler automatically takes care of sending back heartbeats and
// updating state on workflows for an activity task. tickMinInterval determines
// the max rate at which the CoordinatedActivityHandler.Tick function will be
// called.
//
// For example, when the Tick function returns quickly (e.g.: noop), and
// tickMinInterval is 1 * time.Second, Tick is guaranteed to be called at most
// once per second. The rate can be slower if Tick takes more than
// tickMinInterval to complete.
func (w *ActivityWorker) AddCoordinatedHandler(heartbeatInterval, tickMinInterval time.Duration, handler *CoordinatedActivityHandler) {
	adapter := &coordinatedActivityAdapter{
		heartbeatInterval: heartbeatInterval,
		tickMinInterval:   tickMinInterval,
		worker:            w,
		handler:           handler,
	}
	w.AddHandler(&ActivityHandler{
		Activity:    handler.Activity,
		HandlerFunc: adapter.coordinate,
		Input:       handler.Input,
	})
}

type coordinatedActivityAdapter struct {
	heartbeatInterval time.Duration
	tickMinInterval   time.Duration
	worker            *ActivityWorker
	handler           *CoordinatedActivityHandler
}

func (c *coordinatedActivityAdapter) heartbeat(activityTask *swf.PollForActivityTaskOutput, stop <-chan struct{}, cancelActivity chan error) {
	heartbeats := time.NewTicker(c.heartbeatInterval)
	defer heartbeats.Stop()
	for {
		select {
		case <-heartbeats.C:
			if status, err := c.worker.SWF.RecordActivityTaskHeartbeat(&swf.RecordActivityTaskHeartbeatInput{
				TaskToken: activityTask.TaskToken,
			}); err != nil {
				if ae, ok := err.(awserr.Error); ok && ae.Code() == ErrorTypeUnknownResourceFault && strings.Contains(ae.Message(), TaskGone) {
					log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=activity-gone", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID))
					cancelActivity <- nil
					return
				}
				log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=heartbeat-error error=%s ", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID), err.Error())
			} else {
				log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=heartbeat-recorded", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID))
				if *status.CancelRequested {
					log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=activity-cancel-requested", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID))
					cancelActivity <- ActivityTaskCanceledError{}
					return
				}
			}
		case <-stop:
			return
		}
	}
}

func (c *coordinatedActivityAdapter) coordinate(activityTask *swf.PollForActivityTaskOutput, input interface{}) (interface{}, error) {
	update, err := c.handler.Start(activityTask, input)
	if err != nil {
		return nil, err
	}
	if err := c.worker.signalStart(activityTask, update); err != nil {
		return nil, err
	}

	cancel := make(chan error)
	stopHeartbeating := make(chan struct{})

	go c.heartbeat(activityTask, stopHeartbeating, cancel)
	defer close(stopHeartbeating)

	ticks := time.NewTicker(c.tickMinInterval)
	defer ticks.Stop()
	for {
		select {
		case cause := <-cancel:
			if err := c.handler.Cancel(activityTask, input); err != nil {
				log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=activity-cancel-err err=%q", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID), err)
			}
			return nil, cause
		case <-ticks.C:
			cont, res, err := c.handler.Tick(activityTask, input)
			if !cont {
				return res, err
			}
			if res != nil {
				//send an activity update when the result is not null, but we are continuing
				if err := c.worker.signalUpdate(activityTask, res); err != nil {
					log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=signal-update-error error=%q", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID), err)
					cancel <- err
					continue // go pick up the cancel message
				}
				log.Printf("workflow-id=%s activity-id=%s activity-id=%s at=signal-update", LS(activityTask.WorkflowExecution.WorkflowID), LS(activityTask.ActivityType.Name), LS(activityTask.ActivityID))
			}
		}
	}
}
