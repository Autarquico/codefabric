package domain

import "time"

// EventType lists the minimum events of §20.1.
type EventType string

// Event types.
const (
	EventWorkerJoined      EventType = "WorkerJoined"
	EventWorkerHeartbeat   EventType = "WorkerHeartbeat" // never persisted in the event store
	EventWorkerLost        EventType = "WorkerLost"
	EventWorkerReconnected EventType = "WorkerReconnected"
	EventWorkerRestarted   EventType = "WorkerRestarted"
	EventWorkerDraining    EventType = "WorkerDraining"
	EventWorkerDrained     EventType = "WorkerDrained"
	EventWorkerRemoved     EventType = "WorkerRemoved"

	EventPlanCreated      EventType = "PlanCreated"
	EventPlanStateChanged EventType = "PlanStateChanged"

	EventTaskCreated      EventType = "TaskCreated"
	EventTaskStateChanged EventType = "TaskStateChanged"
	EventTaskNeedsInput   EventType = "TaskNeedsInput"
	EventInputProvided    EventType = "InputProvided"

	EventRunQueued               EventType = "RunQueued"
	EventRunBlockedByBudget      EventType = "RunBlockedByBudget"
	EventRunAssigned             EventType = "RunAssigned"
	EventLeaseAccepted           EventType = "LeaseAccepted"
	EventLeaseExpired            EventType = "LeaseExpired" // never accepted: timed out, or its Run ended first
	EventLeaseRejected           EventType = "LeaseRejected"
	EventRunStarted              EventType = "RunStarted"
	EventRunCheckpointed         EventType = "RunCheckpointed"
	EventRunCheckpointFailed     EventType = "RunCheckpointFailed"
	EventRunWaitingInput         EventType = "RunWaitingInput"
	EventRunSuspended            EventType = "RunSuspended"
	EventRunCompleted            EventType = "RunCompleted"
	EventRunFailed               EventType = "RunFailed"
	EventRunCancelled            EventType = "RunCancelled"
	EventRunStopped              EventType = "RunStopped" // the Worker confirms that no process of the Run is left (§10.7)
	EventRunLost                 EventType = "RunLost"
	EventRunOrphaned             EventType = "RunOrphaned"
	EventRunForked               EventType = "RunForked"
	EventRunContinued            EventType = "RunContinued"
	EventRunPromoted             EventType = "RunPromoted"
	EventRunSealed               EventType = "RunSealed" // the Worker sealed the result into a commit (§22.6)
	EventRunPublished            EventType = "RunPublished"
	EventRunPublicationFailed    EventType = "RunPublicationFailed"
	EventRunPublicationRequested EventType = "RunPublicationRequested" // `cf run publish` asked for another try
	EventJournalTruncated        EventType = "JournalTruncated"

	EventWorkflowAdopted       EventType = "WorkflowAdopted"
	EventWorkflowCommitted     EventType = "WorkflowCommitted"
	EventWorkflowConflict      EventType = "WorkflowConflict"
	EventWorkflowApplied       EventType = "WorkflowApplied"
	EventWorkflowDriftDetected EventType = "WorkflowDriftDetected"

	EventModelUsageRecorded    EventType = "ModelUsageRecorded"
	EventProviderStatusChanged EventType = "ProviderStatusChanged"
	EventBudgetWarning         EventType = "BudgetWarning"
	EventBudgetExceeded        EventType = "BudgetExceeded"

	EventConfirmationRequested EventType = "ConfirmationRequested"
	EventConfirmationResolved  EventType = "ConfirmationResolved"

	EventArtifactCreated     EventType = "ArtifactCreated"
	EventEvaluationStarted   EventType = "EvaluationStarted"
	EventEvaluationCompleted EventType = "EvaluationCompleted"
)

// ActorKind says who caused an Event.
type ActorKind string

// Actor kinds.
const (
	ActorHuman  ActorKind = "human"
	ActorMCP    ActorKind = "mcp"
	ActorSystem ActorKind = "system"
	ActorWorker ActorKind = "worker"
)

// ActorRef identifies the actor of an Event.
type ActorRef struct {
	Kind ActorKind
	ID   string
}

// Event is §20.2. Unset IDs and strings are empty; WorkerSeq is nil when the
// Controller emitted the event.
type Event struct {
	ID            ID
	SchemaVersion int
	Sequence      uint64
	Timestamp     time.Time
	ReceivedAt    time.Time
	Source        string
	WorkerEpoch   string
	WorkerSeq     *uint64
	RequestID     string
	Type          EventType
	Level         string
	Actor         ActorRef
	ProjectID     ID
	PlanID        ID
	TaskID        ID
	RunID         ID
	WorkerID      ID
	WorkflowID    ID
	Message       string
	Data          map[string]any
}
