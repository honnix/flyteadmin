package interfaces

// Holds details about a queue used for task execution.
// Matching attributes determine which workflows' tasks will run where.
type ExecutionQueue struct {
	Primary    string
	Dynamic    string
	Attributes []string
}

type ExecutionQueues []ExecutionQueue

// Defines the specific resource attributes (tags) a workflow requires to run.
type WorkflowConfig struct {
	Project      string   `json:"project"`
	Domain       string   `json:"domain"`
	WorkflowName string   `json:"workflowName"`
	Tags         []string `json:"tags"`
}

type WorkflowConfigs []WorkflowConfig

type QueueConfig struct {
	ExecutionQueues ExecutionQueues `json:"executionQueues"`
	WorkflowConfigs WorkflowConfigs `json:"workflowConfigs"`
}

// Provides values set in runtime configuration files.
// These files can be changed without requiring a full server restart.
type QueueConfiguration interface {
	// Returns executions queues defined in runtime configuration files.
	GetExecutionQueues() []ExecutionQueue
	// Returns workflow configurations defined in runtime configuration files.
	GetWorkflowConfigs() []WorkflowConfig
}
