package impl

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	dataInterfaces "github.com/lyft/flyteadmin/pkg/data/interfaces"
	"github.com/lyft/flytestdlib/promutils"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/lyft/flyteadmin/pkg/common"

	"github.com/lyft/flytestdlib/logger"
	"github.com/lyft/flytestdlib/storage"

	"github.com/lyft/flyteadmin/pkg/async/notifications"
	notificationInterfaces "github.com/lyft/flyteadmin/pkg/async/notifications/interfaces"
	"github.com/lyft/flyteadmin/pkg/errors"
	"github.com/lyft/flyteadmin/pkg/manager/impl/executions"
	"github.com/lyft/flyteadmin/pkg/manager/impl/util"
	"github.com/lyft/flyteadmin/pkg/manager/impl/validation"
	"github.com/lyft/flyteadmin/pkg/manager/interfaces"
	"github.com/lyft/flyteadmin/pkg/repositories"
	repositoryInterfaces "github.com/lyft/flyteadmin/pkg/repositories/interfaces"
	"github.com/lyft/flyteadmin/pkg/repositories/models"
	"github.com/lyft/flyteadmin/pkg/repositories/transformers"
	runtimeInterfaces "github.com/lyft/flyteadmin/pkg/runtime/interfaces"
	workflowengineInterfaces "github.com/lyft/flyteadmin/pkg/workflowengine/interfaces"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/admin"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"
	"google.golang.org/grpc/codes"

	"github.com/benbjohnson/clock"
	"github.com/golang/protobuf/proto"
	"github.com/lyft/flyteadmin/pkg/manager/impl/shared"
)

const parentContainerQueueKey = "parent_queue"
const childContainerQueueKey = "child_queue"
const noSourceExecutionID = 0

// Map of [project] -> map of [domain] -> stop watch
type projectDomainScopedStopWatchMap = map[string]map[string]*promutils.StopWatch

type executionSystemMetrics struct {
	Scope                    promutils.Scope
	ActiveExecutions         prometheus.Gauge
	ExecutionsCreated        prometheus.Counter
	ExecutionsTerminated     prometheus.Counter
	ExecutionEventsCreated   prometheus.Counter
	PropellerFailures        prometheus.Counter
	PublishNotificationError prometheus.Counter
	TransformerError         prometheus.Counter
	UnexpectedDataError      prometheus.Counter
	SpecSizeBytes            prometheus.Summary
	ClosureSizeBytes         prometheus.Summary
	AcceptanceDelay          prometheus.Summary
}

type executionUserMetrics struct {
	Scope                      promutils.Scope
	ScheduledExecutionDelays   projectDomainScopedStopWatchMap
	WorkflowExecutionDurations projectDomainScopedStopWatchMap
}

type ExecutionManager struct {
	db                 repositories.RepositoryInterface
	config             runtimeInterfaces.Configuration
	storageClient      *storage.DataStore
	workflowExecutor   workflowengineInterfaces.Executor
	queueAllocator     executions.QueueAllocator
	_clock             clock.Clock
	systemMetrics      executionSystemMetrics
	userMetrics        executionUserMetrics
	notificationClient notificationInterfaces.Publisher
	urlData            dataInterfaces.RemoteURLInterface
}

func (m *ExecutionManager) populateExecutionQueue(
	ctx context.Context, identifier core.Identifier, compiledWorkflow *core.CompiledWorkflowClosure) {
	queueConfig := m.queueAllocator.GetQueue(ctx, identifier)
	for _, task := range compiledWorkflow.Tasks {
		container := task.Template.GetContainer()
		if container == nil {
			// Unrecognized target type, nothing to do
			continue
		}
		if queueConfig.PrimaryQueue != "" {
			logger.Debugf(ctx, "Assigning %s as parent queue for task %+v", queueConfig.PrimaryQueue, task.Template.Id)
			container.Config = append(container.Config, &core.KeyValuePair{
				Key:   parentContainerQueueKey,
				Value: queueConfig.PrimaryQueue,
			})
		}

		if queueConfig.DynamicQueue != "" {
			logger.Debugf(ctx, "Assigning %s as child queue for task %+v", queueConfig.DynamicQueue, task.Template.Id)
			container.Config = append(container.Config, &core.KeyValuePair{
				Key:   childContainerQueueKey,
				Value: queueConfig.DynamicQueue,
			})
		}
	}
}

func validateMapSize(maxEntries int, candidate map[string]string, candidateName string) error {
	if maxEntries == 0 {
		// Treat the max as unset
		return nil
	}
	if len(candidate) > maxEntries {
		return errors.NewFlyteAdminErrorf(codes.InvalidArgument, "%s has too many entries [%v > %v]",
			candidateName, len(candidate), maxEntries)
	}
	return nil
}

// Labels and annotations defined in the execution spec are preferred over those defined in the
// reference launch plan spec.
func (m *ExecutionManager) addLabelsAndAnnotations(requestSpec *admin.ExecutionSpec,
	partiallyPopulatedInputs *workflowengineInterfaces.ExecuteWorkflowInput) error {

	var labels map[string]string
	if requestSpec.Labels != nil && requestSpec.Labels.Values != nil {
		labels = requestSpec.Labels.Values
	} else if partiallyPopulatedInputs.Reference.Spec.Labels != nil &&
		partiallyPopulatedInputs.Reference.Spec.Labels.Values != nil {
		labels = partiallyPopulatedInputs.Reference.Spec.Labels.Values
	}

	var annotations map[string]string
	if requestSpec.Annotations != nil && requestSpec.Annotations.Values != nil {
		annotations = requestSpec.Annotations.Values
	} else if partiallyPopulatedInputs.Reference.Spec.Annotations != nil &&
		partiallyPopulatedInputs.Reference.Spec.Annotations.Values != nil {
		annotations = partiallyPopulatedInputs.Reference.Spec.Annotations.Values
	}

	err := validateMapSize(m.config.RegistrationValidationConfiguration().GetMaxLabelEntries(), labels, "Labels")
	if err != nil {
		return err
	}
	err = validateMapSize(
		m.config.RegistrationValidationConfiguration().GetMaxAnnotationEntries(), annotations, "Annotations")
	if err != nil {
		return err
	}

	partiallyPopulatedInputs.Labels = labels
	partiallyPopulatedInputs.Annotations = annotations
	return nil
}

func (m *ExecutionManager) offloadInputs(ctx context.Context, literalMap *core.LiteralMap, identifier *core.WorkflowExecutionIdentifier, key string) (storage.DataReference, error) {
	if literalMap == nil {
		literalMap = &core.LiteralMap{}
	}
	inputsURI, err := m.storageClient.ConstructReference(ctx, m.storageClient.GetBaseContainerFQN(ctx), shared.Metadata, identifier.Project, identifier.Domain, identifier.Name, key)
	if err != nil {
		return "", err
	}
	if err := m.storageClient.WriteProtobuf(ctx, inputsURI, storage.Options{}, literalMap); err != nil {
		return "", err
	}
	return inputsURI, nil
}

func (m *ExecutionManager) launchExecutionAndPrepareModel(
	ctx context.Context, request admin.ExecutionCreateRequest, requestedAt time.Time) (*models.Execution, error) {
	err := validation.ValidateExecutionRequest(ctx, request, m.db, m.config.ApplicationConfiguration())
	if err != nil {
		logger.Debugf(ctx, "Failed to validate ExecutionCreateRequest %+v with err %v", request, err)
		return nil, err
	}
	launchPlanModel, err := util.GetLaunchPlanModel(ctx, m.db, *request.Spec.LaunchPlan)
	if err != nil {
		logger.Debugf(ctx, "Failed to get launch plan model for ExecutionCreateRequest %+v with err %v", request, err)
		return nil, err
	}
	launchPlan, err := transformers.FromLaunchPlanModel(launchPlanModel)
	if err != nil {
		logger.Debugf(ctx, "Failed to transform launch plan model %+v with err %v", launchPlanModel, err)
		return nil, err
	}
	executionInputs, err := validation.CheckAndFetchInputsForExecution(
		request.Inputs,
		launchPlan.Spec.FixedInputs,
		launchPlan.Closure.ExpectedInputs,
	)

	if err != nil {
		logger.Debugf(ctx, "Failed to CheckAndFetchInputsForExecution with request.Inputs: %+v"+
			"fixed inputs: %+v and expected inputs: %+v with err %v",
			request.Inputs, launchPlan.Spec.FixedInputs, launchPlan.Closure.ExpectedInputs, err)
		return nil, err
	}
	workflow, err := util.GetWorkflow(ctx, m.db, m.storageClient, *launchPlan.Spec.WorkflowId)
	if err != nil {
		logger.Debugf(ctx, "Failed to get workflow with id %+v with err %v", launchPlan.Spec.WorkflowId, err)
		return nil, err
	}
	name := util.GetExecutionName(request)
	workflowExecutionID := core.WorkflowExecutionIdentifier{
		Project: request.Project,
		Domain:  request.Domain,
		Name:    name,
	}

	// Get the node execution (if any) that launched this execution
	var parentNodeExecutionID uint
	if request.Spec.Metadata != nil && request.Spec.Metadata.ParentNodeExecution != nil {
		parentNodeExecutionModel, err := util.GetNodeExecutionModel(ctx, m.db, request.Spec.Metadata.ParentNodeExecution)
		if err != nil {
			logger.Errorf(ctx, "Failed to get node execution [%+v] that launched this execution [%+v] with error %v",
				request.Spec.Metadata.ParentNodeExecution, workflowExecutionID, err)
			return nil, err
		}

		parentNodeExecutionID = parentNodeExecutionModel.ID
	}

	// Dynamically assign task resource defaults.
	for _, task := range workflow.Closure.CompiledWorkflow.Tasks {
		validation.SetDefaults(ctx, m.config.TaskResourceConfiguration(), task)
	}

	// Dynamically assign execution queues.
	m.populateExecutionQueue(ctx, *workflow.Id, workflow.Closure.CompiledWorkflow)

	inputsURI, err := m.offloadInputs(ctx, executionInputs, &workflowExecutionID, shared.Inputs)
	if err != nil {
		return nil, err
	}
	userInputsURI, err := m.offloadInputs(ctx, request.Inputs, &workflowExecutionID, shared.UserInputs)
	if err != nil {
		return nil, err
	}

	// TODO: Reduce CRD size and use offloaded input URI to blob store instead.
	executeWorkflowInputs := workflowengineInterfaces.ExecuteWorkflowInput{
		ExecutionID: &workflowExecutionID,
		WfClosure:   *workflow.Closure.CompiledWorkflow,
		Inputs:      executionInputs,
		Reference:   *launchPlan,
		AcceptedAt:  requestedAt,
	}
	err = m.addLabelsAndAnnotations(request.Spec, &executeWorkflowInputs)
	if err != nil {
		return nil, err
	}

	execInfo, err := m.workflowExecutor.ExecuteWorkflow(ctx, executeWorkflowInputs)
	if err != nil {
		m.systemMetrics.PropellerFailures.Inc()
		logger.Infof(ctx, "Failed to execute workflow %+v with execution id %+v and inputs %+v with err %v",
			request, workflowExecutionID, executionInputs, err)
		return nil, err
	}
	executionCreatedAt := time.Now()
	acceptanceDelay := executionCreatedAt.Sub(requestedAt)
	m.systemMetrics.AcceptanceDelay.Observe(acceptanceDelay.Seconds())

	// Request notification settings takes precedence over the launch plan settings.
	// If there is no notification in the request and DisableAll is not true, use the settings from the launch plan.
	var notificationsSettings []*admin.Notification
	if launchPlan.Spec.GetEntityMetadata() != nil {
		notificationsSettings = launchPlan.Spec.EntityMetadata.GetNotifications()
	}
	if request.Spec.GetNotifications() != nil && request.Spec.GetNotifications().Notifications != nil &&
		len(request.Spec.GetNotifications().Notifications) > 0 {
		notificationsSettings = request.Spec.GetNotifications().Notifications
	} else if request.Spec.GetDisableAll() {
		notificationsSettings = make([]*admin.Notification, 0)
	}

	executionModel, err := transformers.CreateExecutionModel(transformers.CreateExecutionModelInput{
		WorkflowExecutionID: workflowExecutionID,
		RequestSpec:         request.Spec,
		LaunchPlanID:        launchPlanModel.ID,
		WorkflowID:          launchPlanModel.WorkflowID,
		// The execution is not considered running until the propeller sends a specific event saying so.
		Phase:                 core.WorkflowExecution_UNDEFINED,
		CreatedAt:             m._clock.Now(),
		Notifications:         notificationsSettings,
		WorkflowIdentifier:    workflow.Id,
		ParentNodeExecutionID: parentNodeExecutionID,
		Cluster:               execInfo.Cluster,
		InputsURI:             inputsURI,
		UserInputsURI:         userInputsURI,
	})
	if err != nil {
		logger.Infof(ctx, "Failed to create execution model in transformer for id: [%+v] with err: %v",
			workflowExecutionID, err)
		return nil, err
	}
	return executionModel, nil
}

// Inserts an execution model into the database store and emits platform metrics.
func (m *ExecutionManager) createExecutionModel(
	ctx context.Context, executionModel *models.Execution) (*core.WorkflowExecutionIdentifier, error) {
	workflowExecutionIdentifier := core.WorkflowExecutionIdentifier{
		Project: executionModel.ExecutionKey.Project,
		Domain:  executionModel.ExecutionKey.Domain,
		Name:    executionModel.ExecutionKey.Name,
	}
	err := m.db.ExecutionRepo().Create(ctx, *executionModel)
	if err != nil {
		logger.Debugf(ctx, "failed to save newly created execution [%+v] with id %+v to db with err %v",
			workflowExecutionIdentifier, workflowExecutionIdentifier, err)
		return nil, err
	}
	m.systemMetrics.ActiveExecutions.Inc()
	m.systemMetrics.ExecutionsCreated.Inc()
	m.systemMetrics.SpecSizeBytes.Observe(float64(len(executionModel.Spec)))
	m.systemMetrics.ClosureSizeBytes.Observe(float64(len(executionModel.Closure)))
	return &workflowExecutionIdentifier, nil
}

func (m *ExecutionManager) CreateExecution(
	ctx context.Context, request admin.ExecutionCreateRequest, requestedAt time.Time) (
	*admin.ExecutionCreateResponse, error) {
	// Prior to  flyteidl v0.15.0, Inputs was held in ExecutionSpec. Ensure older clients continue to work.
	if request.Inputs == nil || len(request.Inputs.Literals) == 0 {
		request.Inputs = request.GetSpec().GetInputs()
	}
	executionModel, err := m.launchExecutionAndPrepareModel(ctx, request, requestedAt)
	if err != nil {
		return nil, err
	}
	workflowExecutionIdentifier, err := m.createExecutionModel(ctx, executionModel)
	if err != nil {
		return nil, err
	}
	return &admin.ExecutionCreateResponse{
		Id: workflowExecutionIdentifier,
	}, nil
}

func (m *ExecutionManager) RelaunchExecution(
	ctx context.Context, request admin.ExecutionRelaunchRequest, requestedAt time.Time) (
	*admin.ExecutionCreateResponse, error) {
	existingExecutionModel, err := util.GetExecutionModel(ctx, m.db, *request.Id)
	if err != nil {
		logger.Debugf(ctx, "Failed to get execution model for request [%+v] with err %v", request, err)
		return nil, err
	}
	existingExecution, err := transformers.FromExecutionModel(*existingExecutionModel)
	if err != nil {
		return nil, err
	}

	executionSpec := existingExecution.Spec
	if executionSpec.Metadata == nil {
		executionSpec.Metadata = &admin.ExecutionMetadata{}
	}
	var inputs *core.LiteralMap
	if len(existingExecutionModel.UserInputsURI) > 0 {
		inputs = &core.LiteralMap{}
		if err := m.storageClient.ReadProtobuf(ctx, existingExecutionModel.UserInputsURI, inputs); err != nil {
			return nil, err
		}
	} else {
		// For old data, inputs are held in the spec
		var spec admin.ExecutionSpec
		err = proto.Unmarshal(existingExecutionModel.Spec, &spec)
		if err != nil {
			return nil, errors.NewFlyteAdminErrorf(codes.Internal, "failed to unmarshal spec")
		}
		inputs = spec.Inputs
	}
	executionSpec.Metadata.Mode = admin.ExecutionMetadata_RELAUNCH
	executionModel, err := m.launchExecutionAndPrepareModel(ctx, admin.ExecutionCreateRequest{
		Project: request.Id.Project,
		Domain:  request.Id.Domain,
		Name:    request.Name,
		Spec:    executionSpec,
		Inputs:  inputs,
	}, requestedAt)
	if err != nil {
		return nil, err
	}
	executionModel.SourceExecutionID = existingExecutionModel.ID
	workflowExecutionIdentifier, err := m.createExecutionModel(ctx, executionModel)
	if err != nil {
		return nil, err
	}
	logger.Debugf(ctx, "Successfully relaunched [%+v] as [%+v]", request.Id, workflowExecutionIdentifier)
	return &admin.ExecutionCreateResponse{
		Id: workflowExecutionIdentifier,
	}, nil
}

func (m *ExecutionManager) emitScheduledWorkflowMetrics(
	ctx context.Context, executionModel *models.Execution, runningEventTimeProto *timestamp.Timestamp) {
	if executionModel == nil || runningEventTimeProto == nil {
		logger.Warningf(context.Background(),
			"tried to calculate scheduled workflow execution stats with a nil execution or event time")
		return
	}
	// Find the reference launch plan to get the kickoff time argument
	execution, err := transformers.FromExecutionModel(*executionModel)
	if err != nil {
		logger.Warningf(context.Background(),
			"failed to transform execution model when emitting scheduled workflow execution stats with for "+
				"[%s/%s/%s]", executionModel.Project, executionModel.Domain, executionModel.Name)
		return
	}
	launchPlan, err := util.GetLaunchPlan(context.Background(), m.db, *execution.Spec.LaunchPlan)
	if err != nil {
		logger.Warningf(context.Background(),
			"failed to find launch plan when emitting scheduled workflow execution stats with for "+
				"execution: [%+v] and launch plan [%+v]", execution.Id, execution.Spec.LaunchPlan)
		return
	}

	if launchPlan.Spec.EntityMetadata == nil ||
		launchPlan.Spec.EntityMetadata.Schedule == nil ||
		launchPlan.Spec.EntityMetadata.Schedule.KickoffTimeInputArg == "" {
		// Kickoff time arguments aren't always required for scheduled workflows.
		logger.Debugf(context.Background(), "no kickoff time to report for scheduled workflow execution [%+v]",
			execution.Id)
		return
	}

	var inputs core.LiteralMap
	err = m.storageClient.ReadProtobuf(ctx, executionModel.InputsURI, &inputs)
	if err != nil {
		logger.Errorf(ctx, "Failed to find inputs for emitting schedule delay event from uri: [%v]", executionModel.InputsURI)
		return
	}
	scheduledKickoffTimeProto := inputs.Literals[launchPlan.Spec.EntityMetadata.Schedule.KickoffTimeInputArg]
	if scheduledKickoffTimeProto == nil || scheduledKickoffTimeProto.GetScalar() == nil ||
		scheduledKickoffTimeProto.GetScalar().GetPrimitive() == nil ||
		scheduledKickoffTimeProto.GetScalar().GetPrimitive().GetDatetime() == nil {
		logger.Warningf(context.Background(),
			"failed to find scheduled kickoff time datetime value for scheduled workflow execution [%+v] "+
				"although one was expected", execution.Id)
		return
	}
	scheduledKickoffTime, err := ptypes.Timestamp(scheduledKickoffTimeProto.GetScalar().GetPrimitive().GetDatetime())
	if err != nil {
		// Timestamps are serialized by flyteadmin and should always be valid
		return
	}
	runningEventTime, err := ptypes.Timestamp(runningEventTimeProto)
	if err != nil {
		// Timestamps are always sent from propeller and should always be valid
		return
	}

	domainCounterMap, ok := m.userMetrics.ScheduledExecutionDelays[execution.Id.Project]
	if !ok {
		domainCounterMap = make(map[string]*promutils.StopWatch)
		m.userMetrics.ScheduledExecutionDelays[execution.Id.Project] = domainCounterMap
	}

	var watch *promutils.StopWatch
	watch, ok = domainCounterMap[execution.Id.Domain]
	if !ok {
		newWatch, err := m.systemMetrics.Scope.NewSubScope(execution.Id.Project).NewSubScope(execution.Id.Domain).NewStopWatch(
			"scheduled_execution_delay",
			"delay between scheduled execution time and time execution was observed running",
			time.Nanosecond)
		if err != nil {
			// Could be related to a concurrent exception.
			logger.Debugf(context.Background(),
				"failed to emit scheduled workflow execution delay stat, couldn't find or create counter")
			return
		}
		watch = &newWatch
		domainCounterMap[execution.Id.Domain] = watch
	}
	watch.Observe(scheduledKickoffTime, runningEventTime)
}

func (m *ExecutionManager) emitOverallWorkflowExecutionTime(
	executionModel *models.Execution, terminalEventTimeProto *timestamp.Timestamp) {
	if executionModel == nil || terminalEventTimeProto == nil {
		logger.Warningf(context.Background(),
			"tried to calculate scheduled workflow execution stats with a nil execution or event time")
		return
	}

	domainCounterMap, ok := m.userMetrics.WorkflowExecutionDurations[executionModel.Project]
	if !ok {
		domainCounterMap = make(map[string]*promutils.StopWatch)
		m.userMetrics.WorkflowExecutionDurations[executionModel.Project] = domainCounterMap
	}

	var watch *promutils.StopWatch
	watch, ok = domainCounterMap[executionModel.Domain]
	if !ok {
		newWatch, err := m.systemMetrics.Scope.NewSubScope(executionModel.Project).NewSubScope(executionModel.Domain).NewStopWatch(
			"workflow_execution_duration",
			"overall time from when when a workflow create request was sent to k8s to the workflow terminating",
			time.Nanosecond)
		if err != nil {
			// Could be related to a concurrent exception.
			logger.Debugf(context.Background(),
				"failed to emit workflow execution duration stat, couldn't find or create counter")
			return
		}
		watch = &newWatch
		domainCounterMap[executionModel.Domain] = watch
	}

	terminalEventTime, err := ptypes.Timestamp(terminalEventTimeProto)
	if err != nil {
		// Timestamps are always sent from propeller and should always be valid
		return
	}

	if executionModel.ExecutionCreatedAt == nil {
		logger.Warningf(context.Background(), "found execution with nil ExecutionCreatedAt: [%s/%s/%s]",
			executionModel.Project, executionModel.Domain, executionModel.Name)
		return
	}
	watch.Observe(*executionModel.ExecutionCreatedAt, terminalEventTime)
}

func (m *ExecutionManager) CreateWorkflowEvent(ctx context.Context, request admin.WorkflowExecutionEventRequest) (
	*admin.WorkflowExecutionEventResponse, error) {
	err := validation.ValidateCreateWorkflowEventRequest(request)
	if err != nil {
		logger.Debugf(ctx, "received invalid CreateWorkflowEventRequest [%s]: %v", request.RequestId, err)
		return nil, err
	}
	logger.Debugf(ctx, "Received workflow execution event for [%+v] transitioning to phase [%v]",
		request.Event.ExecutionId, request.Event.Phase)

	executionModel, err := util.GetExecutionModel(ctx, m.db, *request.Event.ExecutionId)
	if err != nil {
		logger.Debugf(ctx, "failed to find execution [%+v] for recorded event [%s]: %v",
			request.Event.ExecutionId, request.RequestId, err)
		return nil, err
	}

	wfExecPhase := core.WorkflowExecution_Phase(core.WorkflowExecution_Phase_value[executionModel.Phase])
	if wfExecPhase == request.Event.Phase {
		logger.Debugf(ctx, "This phase %s was already recorded for workflow execution %v",
			wfExecPhase.String(), request.Event.ExecutionId)
		return nil, errors.NewFlyteAdminErrorf(codes.AlreadyExists,
			"This phase %s was already recorded for workflow execution %v",
			wfExecPhase.String(), request.Event.ExecutionId)
	} else if common.IsExecutionTerminal(wfExecPhase) {
		// Cannot go backwards in time from a terminal state to anything else
		curPhase := wfExecPhase.String()
		errorMsg := fmt.Sprintf("Invalid phase change from %s to %s for workflow execution %v", curPhase, request.Event.Phase.String(), request.Event.ExecutionId)
		return nil, errors.NewAlreadyInTerminalStateError(ctx, errorMsg, curPhase)
	}

	err = transformers.UpdateExecutionModelState(executionModel, request, nil)
	if err != nil {
		logger.Debugf(ctx, "failed to transform updated workflow execution model [%+v] after receiving event with err: %v",
			request.Event.ExecutionId, err)
		return nil, err
	}
	executionEventModel, err := transformers.CreateExecutionEventModel(request)
	if err != nil {
		logger.Debugf(ctx, "failed to transform workflow execution event %s for [%+v] after receiving event with err: %v",
			request.RequestId, request.Event.ExecutionId, err)
		return nil, err
	}
	err = m.db.ExecutionRepo().Update(ctx, *executionEventModel, *executionModel)
	if err != nil {
		logger.Debugf(ctx, "Failed to update execution with CreateWorkflowEvent [%+v] with err %v",
			request, err)
		return nil, err
	}

	if request.Event.Phase == core.WorkflowExecution_RUNNING {
		// Workflow executions are created in state "UNDEFINED". All the time up until a RUNNING event is received is
		// considered system-induced delay.
		if executionModel.Mode == int32(admin.ExecutionMetadata_SCHEDULED) {
			go m.emitScheduledWorkflowMetrics(ctx, executionModel, request.Event.OccurredAt)
		}
	} else if common.IsExecutionTerminal(request.Event.Phase) {
		m.systemMetrics.ActiveExecutions.Dec()
		m.systemMetrics.ExecutionsTerminated.Inc()
		go m.emitOverallWorkflowExecutionTime(executionModel, request.Event.OccurredAt)

		err = m.publishNotifications(ctx, request, *executionModel)
		if err != nil {
			// The only errors that publishNotifications will forward are those related
			// to unexpected data and transformation errors.
			logger.Debugf(ctx, "failed to publish notifications for CreateWorkflowEvent [%+v] due to err: %v",
				request, err)
			return nil, err
		}
	}

	m.systemMetrics.ExecutionEventsCreated.Inc()
	return &admin.WorkflowExecutionEventResponse{}, nil
}

func (m *ExecutionManager) GetExecution(
	ctx context.Context, request admin.WorkflowExecutionGetRequest) (*admin.Execution, error) {
	if err := validation.ValidateWorkflowExecutionIdentifier(request.Id); err != nil {
		logger.Debugf(ctx, "GetExecution request [%+v] failed validation with err: %v", request, err)
		return nil, err
	}
	executionModel, err := util.GetExecutionModel(ctx, m.db, *request.Id)
	if err != nil {
		logger.Debugf(ctx, "Failed to get execution model for request [%+v] with err: %v", request, err)
		return nil, err
	}
	var execution *admin.Execution
	var transformerErr error
	if executionModel.SourceExecutionID != noSourceExecutionID {
		// Fetch parent execution to reconstruct its WorkflowExecutionIdentifier
		referenceExecutionModel, err := m.db.ExecutionRepo().GetByID(ctx, executionModel.SourceExecutionID)
		if err != nil {
			logger.Debugf(ctx, "Failed to get reference execution source execution id [%s] for descendant execution [%v]",
				executionModel.SourceExecutionID)
			return nil, err
		}
		referenceExecutionID := transformers.GetExecutionIdentifier(&referenceExecutionModel)
		execution, transformerErr = transformers.FromExecutionModelWithReferenceExecution(*executionModel, &referenceExecutionID)
	} else {
		execution, transformerErr = transformers.FromExecutionModel(*executionModel)
	}
	if transformerErr != nil {
		logger.Debugf(ctx, "Failed to transform execution model [%+v] to proto object with err: %v", request.Id,
			transformerErr)
		return nil, transformerErr
	}

	// TO BE DELETED
	// TODO: Remove the publishing to deprecated fields (Inputs) after a smooth migration has been completed of our existing users
	// For now, publish to deprecated fields thus ensuring old clients don't break when calling GetExecution
	if len(executionModel.InputsURI) > 0 {
		var inputs core.LiteralMap
		if err := m.storageClient.ReadProtobuf(ctx, executionModel.InputsURI, &inputs); err != nil {
			return nil, err
		}
		execution.Closure.ComputedInputs = &inputs
	}
	if len(executionModel.UserInputsURI) > 0 {
		var userInputs core.LiteralMap
		if err := m.storageClient.ReadProtobuf(ctx, executionModel.UserInputsURI, &userInputs); err != nil {
			return nil, err
		}
		execution.Spec.Inputs = &userInputs
	}
	// END TO BE DELETED

	return execution, nil
}

func (m *ExecutionManager) GetExecutionData(
	ctx context.Context, request admin.WorkflowExecutionGetDataRequest) (*admin.WorkflowExecutionGetDataResponse, error) {
	executionModel, err := util.GetExecutionModel(ctx, m.db, *request.Id)
	if err != nil {
		logger.Debugf(ctx, "Failed to get execution model for request [%+v] with err: %v", request, err)
		return nil, err
	}
	execution, err := transformers.FromExecutionModel(*executionModel)
	if err != nil {
		logger.Debugf(ctx, "Failed to transform execution model [%+v] to proto object with err: %v", request.Id, err)
		return nil, err
	}
	signedOutputsURLBlob := admin.UrlBlob{}
	if execution.Closure.GetOutputs() != nil && execution.Closure.GetOutputs().GetUri() != "" {
		signedOutputsURLBlob, err = m.urlData.Get(ctx, execution.Closure.GetOutputs().GetUri())
		if err != nil {
			return nil, err
		}
	}
	// Prior to flyteidl v0.15.0, Inputs were held in ExecutionClosure and were not offloaded. Ensure we can return the inputs as expected.
	if len(executionModel.InputsURI) == 0 {
		closure := &admin.ExecutionClosure{}
		// We must not use the FromExecutionModel method because it empties deprecated fields.
		if err := proto.Unmarshal(executionModel.Closure, closure); err != nil {
			return nil, err
		}
		newInputsURI, err := m.offloadInputs(ctx, closure.ComputedInputs, request.Id, shared.Inputs)
		if err != nil {
			return nil, err
		}
		// Update model so as not to offload again.
		executionModel.InputsURI = newInputsURI
		if err := m.db.ExecutionRepo().UpdateExecution(ctx, *executionModel); err != nil {
			return nil, err
		}
	}
	inputsURLBlob, err := m.urlData.Get(ctx, executionModel.InputsURI.String())
	if err != nil {
		return nil, err
	}
	return &admin.WorkflowExecutionGetDataResponse{
		Outputs: &signedOutputsURLBlob,
		Inputs:  &inputsURLBlob,
	}, nil
}

func (m *ExecutionManager) ListExecutions(
	ctx context.Context, request admin.ResourceListRequest) (*admin.ExecutionList, error) {
	// Check required fields
	if err := validation.ValidateResourceListRequest(request); err != nil {
		logger.Debugf(ctx, "ListExecutions request [%+v] failed validation with err: %v", request, err)
		return nil, err
	}
	filters, err := util.GetDbFilters(util.FilterSpec{
		Project:        request.Id.Project,
		Domain:         request.Id.Domain,
		Name:           request.Id.Name, // Optional, may be empty.
		RequestFilters: request.Filters,
	}, common.Execution)
	if err != nil {
		return nil, err
	}
	var sortParameter common.SortParameter
	if request.SortBy != nil {
		sortParameter, err = common.NewSortParameter(*request.SortBy)
		if err != nil {
			return nil, err
		}
	}

	offset, err := validation.ValidateToken(request.Token)
	if err != nil {
		return nil, errors.NewFlyteAdminErrorf(codes.InvalidArgument, "invalid pagination token %s for ListExecutions",
			request.Token)
	}
	listExecutionsInput := repositoryInterfaces.ListResourceInput{
		Limit:         int(request.Limit),
		Offset:        offset,
		InlineFilters: filters,
		SortParameter: sortParameter,
	}
	output, err := m.db.ExecutionRepo().List(ctx, listExecutionsInput)
	if err != nil {
		logger.Debugf(ctx, "Failed to list executions using input [%+v] with err %v", listExecutionsInput, err)
		return nil, err
	}
	executionList, err := transformers.FromExecutionModels(output.Executions)
	if err != nil {
		logger.Errorf(ctx,
			"Failed to transform execution models [%+v] with err: %v", output.Executions, err)
		return nil, err
	}
	// TODO: TO BE DELETED
	// Clear deprecated fields during migration phase. Once migration is complete, these will be cleared in the database.
	// Thus this will be redundant
	for _, execution := range executionList {
		execution.Spec.Inputs = nil
		execution.Closure.ComputedInputs = nil
	}
	// END TO BE DELETED
	var token string
	if len(executionList) == int(request.Limit) {
		token = strconv.Itoa(offset + len(executionList))
	}
	return &admin.ExecutionList{
		Executions: executionList,
		Token:      token,
	}, nil
}

// publishNotifications will only forward major errors because the assumption made is all of the objects
// that are being manipulated have already been validated/manipulated by Flyte itself.
// Note: This method should be refactored somewhere else once the interaction with pushing to SNS.
func (m *ExecutionManager) publishNotifications(ctx context.Context, request admin.WorkflowExecutionEventRequest,
	execution models.Execution) error {
	// Notifications are stored in the Spec object of an admin.Execution object.
	adminExecution, err := transformers.FromExecutionModel(execution)
	if err != nil {
		// This shouldn't happen because execution manager marshaled the data into models.Execution.
		m.systemMetrics.TransformerError.Inc()
		return errors.NewFlyteAdminErrorf(codes.Internal, "Failed to transform execution [%+v] with err: %v", request.Event.ExecutionId, err)
	}
	var notificationsList = adminExecution.Closure.Notifications
	logger.Debugf(ctx, "publishing notifications for execution [%+v] in state [%+v] for notifications [%+v]",
		request.Event.ExecutionId, request.Event.Phase, notificationsList)
	for _, notification := range notificationsList {
		// Check if the notification phase matches the current one.
		var matchPhase = false
		for _, phase := range notification.Phases {
			if phase == request.Event.Phase {
				matchPhase = true
			}
		}

		// The current phase doesn't match; no notifications will be sent for the current notification option.
		if !matchPhase {
			continue
		}

		// Currently all three supported notifications use email underneath to send the notification.
		// Convert Slack and PagerDuty into an EmailNotification type.
		var emailNotification admin.EmailNotification
		if notification.GetEmail() != nil {
			emailNotification.RecipientsEmail = notification.GetEmail().GetRecipientsEmail()
		} else if notification.GetPagerDuty() != nil {
			emailNotification.RecipientsEmail = notification.GetPagerDuty().GetRecipientsEmail()
		} else if notification.GetSlack() != nil {
			emailNotification.RecipientsEmail = notification.GetSlack().GetRecipientsEmail()
		} else {
			logger.Debugf(ctx, "failed to publish notification, encountered unrecognized type: %v", notification.Type)
			m.systemMetrics.UnexpectedDataError.Inc()
			// Unsupported notification types should have been caught when the launch plan was being created.
			return errors.NewFlyteAdminErrorf(codes.Internal, "Unsupported notification type [%v] for execution [%+v]",
				notification.Type, request.Event.ExecutionId)
		}

		// Convert the email Notification into an email message to be published.
		// Currently there are no possible errors while creating an email message.
		// Once customizable content is specified, errors are possible.
		email := notifications.ToEmailMessageFromWorkflowExecutionEvent(
			*m.config.ApplicationConfiguration().GetNotificationsConfig(), emailNotification, request, adminExecution)
		// Errors seen while publishing a message are considered non-fatal to the method and will not result
		// in the method returning an error.
		if err = m.notificationClient.Publish(ctx, proto.MessageName(&emailNotification), email); err != nil {
			m.systemMetrics.PublishNotificationError.Inc()
			logger.Infof(ctx, "error publishing email notification [%+v] with err: [%v]", notification, err)
		}
	}
	return nil
}

func (m *ExecutionManager) TerminateExecution(
	ctx context.Context, request admin.ExecutionTerminateRequest) (*admin.ExecutionTerminateResponse, error) {
	if err := validation.ValidateWorkflowExecutionIdentifier(request.Id); err != nil {
		logger.Debugf(ctx, "received terminate execution request: %v with invalid identifier: %v", request, err)
		return nil, err
	}
	// Save the abort reason (best effort)
	executionModel, err := m.db.ExecutionRepo().Get(ctx, repositoryInterfaces.GetResourceInput{
		Project: request.Id.Project,
		Domain:  request.Id.Domain,
		Name:    request.Id.Name,
	})
	if err != nil {
		logger.Infof(ctx, "couldn't find execution [%+v] to save termination cause", request.Id)
		return nil, err
	}

	err = m.workflowExecutor.TerminateWorkflowExecution(ctx, workflowengineInterfaces.TerminateWorkflowInput{
		ExecutionID: request.Id,
		Cluster:     executionModel.Cluster,
	})
	if err != nil {
		return nil, err
	}

	executionModel.AbortCause = request.Cause
	err = m.db.ExecutionRepo().UpdateExecution(ctx, executionModel)
	if err != nil {
		logger.Debugf(ctx, "failed to save abort cause for terminated execution: %+v with err: %v", request.Id, err)
		return nil, err
	}
	return &admin.ExecutionTerminateResponse{}, nil
}

func newExecutionSystemMetrics(scope promutils.Scope) executionSystemMetrics {
	return executionSystemMetrics{
		Scope: scope,
		ActiveExecutions: scope.MustNewGauge("active_executions",
			"overall count of active workflow executions"),
		ExecutionsCreated: scope.MustNewCounter("executions_created",
			"overall count of successfully completed CreateExecutionRequests"),
		ExecutionsTerminated: scope.MustNewCounter("executions_terminated",
			"overall count of terminated workflow executions"),
		ExecutionEventsCreated: scope.MustNewCounter("execution_events_created",
			"overall count of successfully completed WorkflowExecutionEventRequest"),
		PropellerFailures: scope.MustNewCounter("propeller_failures",
			"propeller failures in creating workflow executions"),
		TransformerError: scope.MustNewCounter("transformer_error",
			"overall count of errors when transforming models and messages"),
		UnexpectedDataError: scope.MustNewCounter("unexpected_data_error",
			"overall count of unexpected data for previously validated objects"),
		PublishNotificationError: scope.MustNewCounter("publish_error",
			"overall count of publish notification errors when invoking publish()"),
		SpecSizeBytes:    scope.MustNewSummary("spec_size_bytes", "size in bytes of serialized execution spec"),
		ClosureSizeBytes: scope.MustNewSummary("closure_size_bytes", "size in bytes of serialized execution closure"),
		AcceptanceDelay: scope.MustNewSummary("acceptance_delay",
			"delay in seconds from when an execution was requested to be created and when it actually was"),
	}
}

func NewExecutionManager(
	db repositories.RepositoryInterface,
	config runtimeInterfaces.Configuration,
	storageClient *storage.DataStore,
	workflowExecutor workflowengineInterfaces.Executor,
	systemScope promutils.Scope,
	userScope promutils.Scope,
	publisher notificationInterfaces.Publisher,
	urlData dataInterfaces.RemoteURLInterface) interfaces.ExecutionInterface {
	queueAllocator := executions.NewQueueAllocator(config)
	systemMetrics := newExecutionSystemMetrics(systemScope)

	userMetrics := executionUserMetrics{
		Scope:                      userScope,
		ScheduledExecutionDelays:   make(map[string]map[string]*promutils.StopWatch),
		WorkflowExecutionDurations: make(map[string]map[string]*promutils.StopWatch),
	}
	return &ExecutionManager{
		db:                 db,
		config:             config,
		storageClient:      storageClient,
		workflowExecutor:   workflowExecutor,
		queueAllocator:     queueAllocator,
		_clock:             clock.New(),
		systemMetrics:      systemMetrics,
		userMetrics:        userMetrics,
		notificationClient: publisher,
		urlData:            urlData,
	}
}
