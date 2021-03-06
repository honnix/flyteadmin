package impl

import (
	"context"
	"strconv"

	"github.com/lyft/flyteadmin/pkg/manager/impl/shared"
	"github.com/lyft/flytestdlib/promutils"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/lyft/flytestdlib/logger"

	"github.com/lyft/flyteadmin/pkg/common"
	"github.com/lyft/flyteadmin/pkg/manager/impl/validation"

	"fmt"

	dataInterfaces "github.com/lyft/flyteadmin/pkg/data/interfaces"
	"github.com/lyft/flyteadmin/pkg/errors"
	"github.com/lyft/flyteadmin/pkg/manager/impl/util"
	"github.com/lyft/flyteadmin/pkg/manager/interfaces"
	"github.com/lyft/flyteadmin/pkg/repositories"
	repoInterfaces "github.com/lyft/flyteadmin/pkg/repositories/interfaces"
	"github.com/lyft/flyteadmin/pkg/repositories/models"
	"github.com/lyft/flyteadmin/pkg/repositories/transformers"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/admin"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"
	"google.golang.org/grpc/codes"
)

type nodeExecutionMetrics struct {
	Scope                      promutils.Scope
	ActiveNodeExecutions       prometheus.Gauge
	NodeExecutionsCreated      prometheus.Counter
	NodeExecutionsTerminated   prometheus.Counter
	NodeExecutionEventsCreated prometheus.Counter
	MissingWorkflowExecution   prometheus.Counter
	ClosureSizeBytes           prometheus.Summary
}

type NodeExecutionManager struct {
	db      repositories.RepositoryInterface
	metrics nodeExecutionMetrics
	urlData dataInterfaces.RemoteURLInterface
}

type updateNodeExecutionStatus int

const (
	updateSucceeded updateNodeExecutionStatus = iota
	updateFailed
	alreadyInTerminalStatus
)

const addIsParentFilter = true

var isParent = common.NewMapFilter(map[string]interface{}{
	shared.ParentTaskExecutionID: nil,
})

func (m *NodeExecutionManager) createNodeExecutionWithEvent(
	ctx context.Context, request *admin.NodeExecutionEventRequest) error {

	var parentTaskExecutionID uint
	if request.Event.ParentTaskMetadata != nil {
		taskExecutionModel, err := util.GetTaskExecutionModel(ctx, m.db, request.Event.ParentTaskMetadata.Id)
		if err != nil {
			return err
		}
		parentTaskExecutionID = taskExecutionModel.ID
	}
	nodeExecutionModel, err := transformers.CreateNodeExecutionModel(transformers.ToNodeExecutionModelInput{
		Request:               request,
		ParentTaskExecutionID: parentTaskExecutionID,
	})
	if err != nil {
		logger.Debugf(ctx, "failed to create node execution model for event request: %s with err: %v",
			request.RequestId, err)
		return err
	}
	nodeExecutionEventModel, err := transformers.CreateNodeExecutionEventModel(*request)
	if err != nil {
		logger.Debugf(ctx, "failed to transform node execution event request: %s into model with err: %v",
			request.RequestId, err)
		return err
	}

	if err := m.db.NodeExecutionRepo().Create(ctx, nodeExecutionEventModel, nodeExecutionModel); err != nil {
		logger.Debugf(ctx, "Failed to create node execution with id [%+v] and model [%+v] "+
			"and event [%+v] with err %v", request.Event.Id, nodeExecutionModel, nodeExecutionEventModel, err)
		return err
	}
	m.metrics.ClosureSizeBytes.Observe(float64(len(nodeExecutionModel.Closure)))
	return nil
}

func (m *NodeExecutionManager) updateNodeExecutionWithEvent(
	ctx context.Context, request *admin.NodeExecutionEventRequest, nodeExecutionModel *models.NodeExecution) (updateNodeExecutionStatus, error) {
	// If we have an existing execution, check if the phase change is valid
	nodeExecPhase := core.NodeExecution_Phase(core.NodeExecution_Phase_value[nodeExecutionModel.Phase])
	if nodeExecPhase == request.Event.Phase {
		logger.Debugf(ctx, "This phase was already recorded %v for %+v", nodeExecPhase.String(), request.Event.Id)
		return updateFailed, errors.NewFlyteAdminErrorf(codes.AlreadyExists,
			"This phase was already recorded %v for %+v", nodeExecPhase.String(), request.Event.Id)
	} else if common.IsNodeExecutionTerminal(nodeExecPhase) {
		// Cannot go from a terminal state to anything else
		logger.Warnf(ctx, "Invalid phase change from %v to %v for node execution %v",
			nodeExecPhase.String(), request.Event.Phase.String(), request.Event.Id)
		return alreadyInTerminalStatus, nil
	}

	// if this node execution kicked off a workflow, validate that the execution exists
	var childExecutionID *core.WorkflowExecutionIdentifier
	if request.Event.GetWorkflowNodeMetadata() != nil {
		childExecutionID = request.Event.GetWorkflowNodeMetadata().ExecutionId
		err := validation.ValidateWorkflowExecutionIdentifier(childExecutionID)
		if err != nil {
			logger.Errorf(ctx, "Invalid execution ID: %s with err: %v",
				childExecutionID, err)
		}
		_, err = util.GetExecutionModel(ctx, m.db, *childExecutionID)
		if err != nil {
			logger.Errorf(ctx, "The node execution launched an execution but it does not exist: %s with err: %v",
				childExecutionID, err)
			return updateFailed, err
		}
	}
	err := transformers.UpdateNodeExecutionModel(request, nodeExecutionModel, childExecutionID)
	if err != nil {
		logger.Debugf(ctx, "failed to update node execution model: %+v with err: %v", request.Event.Id, err)
		return updateFailed, err
	}

	nodeExecutionEventModel, err := transformers.CreateNodeExecutionEventModel(*request)
	if err != nil {
		logger.Debugf(ctx, "failed to create node execution event model for request: %s with err: %v",
			request.RequestId, err)
		return updateFailed, err
	}
	err = m.db.NodeExecutionRepo().Update(ctx, nodeExecutionEventModel, nodeExecutionModel)
	if err != nil {
		logger.Debugf(ctx, "Failed to update node execution with id [%+v] with err %v",
			request.Event.Id, err)
		return updateFailed, err
	}

	return updateSucceeded, nil
}

func (m *NodeExecutionManager) CreateNodeEvent(ctx context.Context, request admin.NodeExecutionEventRequest) (
	*admin.NodeExecutionEventResponse, error) {
	executionID := request.Event.Id.ExecutionId
	logger.Debugf(ctx, "Received node execution event for [%+v] transitioning to phase [%v]",
		executionID, request.Event.Phase)

	_, err := util.GetExecutionModel(ctx, m.db, *executionID)
	if err != nil {
		m.metrics.MissingWorkflowExecution.Inc()
		logger.Debugf(ctx, "Failed to find existing execution with id [%+v] with err: %v", executionID, err)
		if ferr, ok := err.(errors.FlyteAdminError); ok {
			return nil, errors.NewFlyteAdminErrorf(ferr.Code(),
				"Failed to get existing execution id:[%+v] with err: %v", executionID, err)
		}
		return nil, fmt.Errorf("failed to get existing execution id: [%+v] with err: %v", executionID, err)
	}

	nodeExecutionModel, err := m.db.NodeExecutionRepo().Get(ctx, repoInterfaces.GetNodeExecutionInput{
		NodeExecutionIdentifier: *request.Event.Id,
	})
	phase := core.NodeExecution_Phase(core.NodeExecution_Phase_value[nodeExecutionModel.Phase])
	if err != nil {
		if err.(errors.FlyteAdminError).Code() != codes.NotFound {
			logger.Debugf(ctx, "Failed to retrieve existing node execution with id [%+v] with err: %v",
				request.Event.Id, err)
			return nil, err
		}
		err = m.createNodeExecutionWithEvent(ctx, &request)
		if err != nil {
			return nil, err
		}
		m.metrics.NodeExecutionsCreated.Inc()
	} else {
		updateStatus, err := m.updateNodeExecutionWithEvent(ctx, &request, &nodeExecutionModel)
		if err != nil {
			return nil, err
		}

		if updateStatus == alreadyInTerminalStatus {
			curPhase := request.Event.Phase.String()
			errorMsg := fmt.Sprintf("Invalid phase change from %s to %s for node execution %v", phase.String(), curPhase, nodeExecutionModel.ID)
			return nil, errors.NewAlreadyInTerminalStateError(ctx, errorMsg, curPhase)
		}
	}

	if request.Event.Phase == core.NodeExecution_RUNNING {
		m.metrics.ActiveNodeExecutions.Inc()
	} else if common.IsNodeExecutionTerminal(request.Event.Phase) {
		m.metrics.ActiveNodeExecutions.Dec()
		m.metrics.NodeExecutionsTerminated.Inc()
	}
	m.metrics.NodeExecutionEventsCreated.Inc()

	return &admin.NodeExecutionEventResponse{}, nil
}

func (m *NodeExecutionManager) GetNodeExecution(
	ctx context.Context, request admin.NodeExecutionGetRequest) (*admin.NodeExecution, error) {
	if err := validation.ValidateNodeExecutionIdentifier(request.Id); err != nil {
		logger.Debugf(ctx, "get node execution called with invalid identifier [%+v]: %v", request.Id, err)
	}
	nodeExecutionModel, err := util.GetNodeExecutionModel(ctx, m.db, request.Id)
	if err != nil {
		logger.Debugf(ctx, "Failed to get node execution with id [%+v] with err %v",
			request.Id, err)
		return nil, err
	}
	nodeExecution, err := transformers.FromNodeExecutionModel(*nodeExecutionModel)
	if err != nil {
		logger.Debugf(ctx, "failed to transform node execution model [%+v] to proto with err: %v", request.Id, err)
		return nil, err
	}
	return nodeExecution, nil
}

func (m *NodeExecutionManager) listNodeExecutions(
	ctx context.Context, identifierFilters []common.InlineFilter,
	requestFilters string, limit uint32, requestToken string, sortBy *admin.Sort, addIsParentFilter bool) (
	*admin.NodeExecutionList, error) {

	filters, err := util.AddRequestFilters(requestFilters, common.NodeExecution, identifierFilters)
	if err != nil {
		return nil, err
	}
	var sortParameter common.SortParameter
	if sortBy != nil {
		sortParameter, err = common.NewSortParameter(*sortBy)
		if err != nil {
			return nil, err
		}
	}
	offset, err := validation.ValidateToken(requestToken)
	if err != nil {
		return nil, errors.NewFlyteAdminErrorf(codes.InvalidArgument,
			"invalid pagination token %s for ListNodeExecutions", requestToken)
	}
	listInput := repoInterfaces.ListResourceInput{
		Limit:         int(limit),
		Offset:        offset,
		InlineFilters: filters,
		SortParameter: sortParameter,
	}
	if addIsParentFilter {
		listInput.MapFilters = []common.MapFilter{
			isParent,
		}
	}
	output, err := m.db.NodeExecutionRepo().List(ctx, listInput)
	if err != nil {
		logger.Debugf(ctx, "Failed to list node executions for request with err %v", err)
		return nil, err
	}

	var token string
	if len(output.NodeExecutions) == int(limit) {
		token = strconv.Itoa(offset + len(output.NodeExecutions))
	}
	nodeExecutionList, err := transformers.FromNodeExecutionModels(output.NodeExecutions)
	if err != nil {
		logger.Debugf(ctx, "failed to transform node execution models for request with err: %v", err)
		return nil, err
	}

	return &admin.NodeExecutionList{
		NodeExecutions: nodeExecutionList,
		Token:          token,
	}, nil
}

func (m *NodeExecutionManager) ListNodeExecutions(
	ctx context.Context, request admin.NodeExecutionListRequest) (*admin.NodeExecutionList, error) {
	// Check required fields
	if err := validation.ValidateNodeExecutionListRequest(request); err != nil {
		return nil, err
	}

	identifierFilters, err := util.GetWorkflowExecutionIdentifierFilters(ctx, *request.WorkflowExecutionId)
	if err != nil {
		return nil, err
	}
	return m.listNodeExecutions(
		ctx, identifierFilters, request.Filters, request.Limit, request.Token, request.SortBy, addIsParentFilter)
}

// Filters on node executions matching the execution parameters (execution project, domain, and name) as well as the
// parent task execution id corresponding to the task execution identified in the request params.
func (m *NodeExecutionManager) ListNodeExecutionsForTask(
	ctx context.Context, request admin.NodeExecutionForTaskListRequest) (*admin.NodeExecutionList, error) {
	// Check required fields
	if err := validation.ValidateNodeExecutionForTaskListRequest(request); err != nil {
		return nil, err
	}
	identifierFilters, err := util.GetWorkflowExecutionIdentifierFilters(
		ctx, *request.TaskExecutionId.NodeExecutionId.ExecutionId)
	if err != nil {
		return nil, err
	}
	parentTaskExecutionModel, err := util.GetTaskExecutionModel(ctx, m.db, request.TaskExecutionId)
	if err != nil {
		return nil, err
	}
	nodeIDFilter, err := common.NewSingleValueFilter(
		common.NodeExecution, common.Equal, shared.ParentTaskExecutionID, parentTaskExecutionModel.ID)
	if err != nil {
		return nil, err
	}
	identifierFilters = append(identifierFilters, nodeIDFilter)
	return m.listNodeExecutions(
		ctx, identifierFilters, request.Filters, request.Limit, request.Token, request.SortBy, !addIsParentFilter)
}

func (m *NodeExecutionManager) GetNodeExecutionData(
	ctx context.Context, request admin.NodeExecutionGetDataRequest) (*admin.NodeExecutionGetDataResponse, error) {
	if err := validation.ValidateNodeExecutionIdentifier(request.Id); err != nil {
		logger.Debugf(ctx, "can't get node execution data with invalid identifier [%+v]: %v", request.Id, err)
	}
	nodeExecutionModel, err := util.GetNodeExecutionModel(ctx, m.db, request.Id)
	if err != nil {
		logger.Debugf(ctx, "Failed to get node execution with id [%+v] with err %v",
			request.Id, err)
		return nil, err
	}
	nodeExecution, err := transformers.FromNodeExecutionModel(*nodeExecutionModel)
	if err != nil {
		logger.Debugf(ctx, "failed to transform node execution model [%+v] when fetching data: %v", request.Id, err)
		return nil, err
	}
	signedInputsURLBlob, err := m.urlData.Get(ctx, nodeExecution.InputUri)
	if err != nil {
		return nil, err
	}
	signedOutputsURLBlob := admin.UrlBlob{}
	if nodeExecution.Closure.GetOutputUri() != "" {
		signedOutputsURLBlob, err = m.urlData.Get(ctx, nodeExecution.Closure.GetOutputUri())
		if err != nil {
			return nil, err
		}
	}
	return &admin.NodeExecutionGetDataResponse{
		Inputs:  &signedInputsURLBlob,
		Outputs: &signedOutputsURLBlob,
	}, nil
}

func NewNodeExecutionManager(
	db repositories.RepositoryInterface, scope promutils.Scope,
	urlData dataInterfaces.RemoteURLInterface) interfaces.NodeExecutionInterface {
	metrics := nodeExecutionMetrics{
		Scope: scope,
		ActiveNodeExecutions: scope.MustNewGauge("active_node_executions",
			"overall count of active node executions"),
		NodeExecutionsCreated: scope.MustNewCounter("node_executions_created",
			"overall count of node executions created"),
		NodeExecutionsTerminated: scope.MustNewCounter("node_executions_terminated",
			"overall count of terminated node executions"),
		NodeExecutionEventsCreated: scope.MustNewCounter("node_execution_events_created",
			"overall count of successfully completed NodeExecutionEventRequest"),
		MissingWorkflowExecution: scope.MustNewCounter("missing_workflow_execution",
			"overall count of node execution events received that are missing a parent workflow execution"),
		ClosureSizeBytes: scope.MustNewSummary("closure_size_bytes",
			"size in bytes of serialized node execution closure"),
	}
	return &NodeExecutionManager{
		db:      db,
		metrics: metrics,
		urlData: urlData,
	}
}
