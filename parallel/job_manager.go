package parallel

import (
	"context"
	"sync"

	"go.uber.org/atomic"

	"github.com/lindb/lindb/aggregation"
	"github.com/lindb/lindb/models"
	"github.com/lindb/lindb/pkg/encoding"
	pb "github.com/lindb/lindb/rpc/proto/common"
	"github.com/lindb/lindb/sql/stmt"
)

//go:generate mockgen -source=./job_manager.go -destination=./job_manager_mock.go -package=parallel

// JobManager represents the job manager for the root broker node
type JobManager interface {
	// SubmitJob submits the distribution query job based on physical plan
	SubmitJob(ctx JobContext) error
	// SubmitMetadataJob submits the distribution metadata query job on physical plan
	SubmitMetadataJob(ctx context.Context, plan *models.PhysicalPlan,
		suggest *stmt.Metadata, resultSet chan []string,
	) (err error)
	// GetJob returns job context by job id
	GetJob(jobID int64) JobContext
	// GetTaskManager return the task manager
	GetTaskManager() TaskManager
}

// jobManager implements the job manager for managing the query job
type jobManager struct {
	taskManager TaskManager

	seq  *atomic.Int64
	jobs sync.Map
}

// NewJobManager creates the job manager
func NewJobManager(taskManger TaskManager) JobManager {
	return &jobManager{
		taskManager: taskManger,
		seq:         atomic.NewInt64(0),
	}
}

// GetJob return the job context by job id
func (j *jobManager) GetJob(jobID int64) JobContext {
	job, ok := j.jobs.Load(jobID)
	if !ok {
		return nil
	}
	jobCtx, ok := job.(JobContext)
	if !ok {
		return nil
	}
	return jobCtx
}

// SubmitJob submits the distribution query job based on physical plan,
// 1. if has intermediate nodes, sends the request to the intermediate nodes
// 2. else sends the request to the leaf node directly
func (j *jobManager) SubmitJob(ctx JobContext) (err error) {
	plan := ctx.Plan()
	planPayload := encoding.JSONMarshal(plan)
	jobID := j.seq.Inc()

	defer func() {
		if err == nil {
			j.jobs.Store(jobID, ctx)
		}
	}()

	taskID := j.taskManager.AllocTaskID()

	// TODO need add param
	req := &pb.TaskRequest{
		JobID:        jobID,
		ParentTaskID: taskID,
		PhysicalPlan: planPayload,
		Payload:      encoding.JSONMarshal(ctx.Query()),
	}
	query := ctx.Query()

	groupAgg := aggregation.NewGroupingAggregator(query.Interval, query.TimeRange, buildAggregatorSpecs(query.FieldNames))
	taskCtx := newTaskContext(taskID, RootTask, "", "", plan.Root.NumOfTask,
		newResultMerger(ctx.Context(), groupAgg, ctx.ResultSet()))
	j.taskManager.Submit(taskCtx)

	if len(plan.Intermediates) > 0 {
		for _, intermediate := range plan.Intermediates {
			if err = j.taskManager.SendRequest(intermediate.Indicator, req); err != nil {
				//TODO kill sent leaf task???
				return err
			}
		}
	} else if len(plan.Leafs) > 0 {
		for _, leaf := range plan.Leafs {
			if err = j.taskManager.SendRequest(leaf.Indicator, req); err != nil {
				//TODO kill sent leaf task???
				return err
			}
		}
	}
	return err
}

// SubmitMetadataJob submits the distribution metadata query job on physical plan
func (j *jobManager) SubmitMetadataJob(ctx context.Context, plan *models.PhysicalPlan,
	suggest *stmt.Metadata, resultSet chan []string,
) (err error) {
	planPayload := encoding.JSONMarshal(plan)
	jobID := j.seq.Inc()

	defer func() {
		if err == nil {
			j.jobs.Store(jobID, ctx)
		}
	}()

	taskID := j.taskManager.AllocTaskID()

	req := &pb.TaskRequest{
		JobID:        jobID,
		RequestType:  pb.RequestType_Metadata,
		ParentTaskID: taskID,
		PhysicalPlan: planPayload,
		Payload:      encoding.JSONMarshal(suggest),
	}

	taskCtx := newTaskContext(taskID, RootTask, "", "", plan.Root.NumOfTask,
		newSuggestResultMerger(resultSet))
	j.taskManager.Submit(taskCtx)

	if len(plan.Leafs) > 0 {
		for _, leaf := range plan.Leafs {
			if err = j.taskManager.SendRequest(leaf.Indicator, req); err != nil {
				//TODO kill sent leaf task???
				return err
			}
		}
	}
	return nil
}

// GetTaskManager return the task manager
func (j *jobManager) GetTaskManager() TaskManager {
	return j.taskManager
}

// buildAggregatorSpecs builds aggregator specs based on field names
func buildAggregatorSpecs(fieldNames []string) aggregation.AggregatorSpecs {
	aggSpecs := make(aggregation.AggregatorSpecs, len(fieldNames))
	for idx, fieldName := range fieldNames {
		aggSpecs[idx] = aggregation.NewAggregatorSpec(fieldName)
	}
	return aggSpecs
}
