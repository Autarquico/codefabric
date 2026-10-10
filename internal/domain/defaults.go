package domain

import "time"

// DefaultValues is Appendix A. Retention (§20.5) is configured elsewhere.
type DefaultValues struct {
	WorkerHeartbeatInterval time.Duration
	WorkerOfflineTTL        time.Duration
	RunLostTimeout          time.Duration
	LeaseAcceptBy           time.Duration
	RunResources            Resources
	WorkerMaxConcurrentRuns int

	JournalMaxBytes      int64
	JournalHardMaxBytes  int64
	JournalFsyncInterval time.Duration
	JournalFsyncEntries  int

	TaskMode       Mode
	TaskCompletion Completion
	TaskRetry      RetryPolicy

	EvaluatorTimeout  time.Duration
	EvaluatorRequired bool

	ForkFrom               string
	CheckpointGrace        time.Duration
	StopGrace              time.Duration
	CheckpointExclude      []string
	PublicationMaxAttempts int
	IdempotencyTTL         time.Duration
	ProjectRepoConfig      RepoConfig

	BudgetWarnAt               float64
	BudgetOnExceed             BudgetExceedAction
	BudgetOnExceedMaxDuration  BudgetExceedAction
	BudgetOnUnobservable       BudgetUnobservableAction
	PromoteStrategy            string
	PromoteRequireConfirmation bool

	ConfirmationTTL      time.Duration
	IdentityCertTTL      time.Duration
	EventsWaitMaxTimeout time.Duration
}

const (
	day      = 24 * time.Hour
	gigabyte = 1 << 30
)

// Defaults returns a fresh copy of the Appendix A values.
func Defaults() DefaultValues {
	return DefaultValues{
		WorkerHeartbeatInterval: 10 * time.Second,
		WorkerOfflineTTL:        45 * time.Second,
		RunLostTimeout:          30 * time.Minute,
		LeaseAcceptBy:           60 * time.Second,
		RunResources:            Resources{CPU: 2, MemoryMB: 4096},
		WorkerMaxConcurrentRuns: 4,

		JournalMaxBytes:      1 * gigabyte,
		JournalHardMaxBytes:  5 * gigabyte,
		JournalFsyncInterval: time.Second,
		JournalFsyncEntries:  64,

		TaskMode:       ModeHeadless,
		TaskCompletion: CompletionFirstSuccess,
		TaskRetry:      RetryPolicy{OnFailed: RetryNone, OnLost: RetryAsk, MaxAttempts: 1},

		EvaluatorTimeout:  15 * time.Minute,
		EvaluatorRequired: true,

		ForkFrom:               "base",
		CheckpointGrace:        30 * time.Second,
		StopGrace:              30 * time.Second,
		CheckpointExclude:      []string{".env*", "*.pem", "*.key"},
		PublicationMaxAttempts: 10,
		IdempotencyTTL:         day,
		ProjectRepoConfig:      RepoConfigAllow,

		BudgetWarnAt:               0.8,
		BudgetOnExceed:             BudgetBlockNew,
		BudgetOnExceedMaxDuration:  BudgetCheckpointStop,
		BudgetOnUnobservable:       BudgetUnobservableWarn,
		PromoteStrategy:            "branch",
		PromoteRequireConfirmation: true,

		ConfirmationTTL:      day,
		IdentityCertTTL:      90 * day,
		EventsWaitMaxTimeout: 10 * time.Minute,
	}
}
