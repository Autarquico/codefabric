package domain

import "time"

// Mode says whether a Run is driven by a person or runs unattended (§7.4).
type Mode string

// Run modes.
const (
	ModeHeadless    Mode = "headless"
	ModeInteractive Mode = "interactive"
)

// Completion is the policy that decides when a Task is done (§11.7).
type Completion string

// Completion policies.
const (
	CompletionFirstSuccess Completion = "first_success"
	CompletionPromote      Completion = "promote"
	CompletionManual       Completion = "manual"
)

// RetryAction is what happens after a Run fails or is lost.
type RetryAction string

// Retry actions.
const (
	RetryNone       RetryAction = "none"
	RetryReschedule RetryAction = "reschedule"
	RetryAsk        RetryAction = "ask"
)

// RepoConfig says whether the repo may carry .codefabric/project.yaml (§3.1).
type RepoConfig string

// Repo config modes.
const (
	RepoConfigAllow RepoConfig = "allow"
	RepoConfigDeny  RepoConfig = "deny"
)

// Creator is the origin of a Plan or Task.
type Creator string

// Creators.
const (
	CreatorHuman      Creator = "human"
	CreatorMCP        Creator = "mcp"
	CreatorAutomation Creator = "automation"
)

// Resources is a CPU and memory request.
type Resources struct {
	CPU      int
	MemoryMB int
}

// RetryPolicy is the per-Task retry configuration (§11.7).
type RetryPolicy struct {
	OnFailed    RetryAction
	OnLost      RetryAction
	MaxAttempts int
}

// BudgetExceedAction is what the Controller does when a budget is exceeded.
type BudgetExceedAction string

// Budget exceed actions.
const (
	BudgetBlockNew       BudgetExceedAction = "block_new"
	BudgetCheckpointStop BudgetExceedAction = "checkpoint_stop"
)

// BudgetUnobservableAction is what happens when usage cannot be observed.
type BudgetUnobservableAction string

// Budget unobservable actions.
const (
	BudgetUnobservableWarn  BudgetUnobservableAction = "warn"
	BudgetUnobservableBlock BudgetUnobservableAction = "block"
)

// Budget limits are optional: a nil limit does not limit (§15).
type Budget struct {
	MaxRuns        *int
	MaxParallel    *int
	MaxTokens      *int64
	MaxCostUSD     *float64
	MaxDuration    *time.Duration
	WarnAt         *float64
	OnExceed       BudgetExceedAction
	OnUnobservable BudgetUnobservableAction
}

// InheritedDefaults are resolved Task -> Plan -> Project -> global (§3.3).
// Zero or nil fields are unset.
type InheritedDefaults struct {
	Mode       Mode
	Resources  *Resources
	Completion Completion
	Retry      *RetryPolicy
	Budget     *Budget
}

// Project groups a repository and its defaults (§3.1).
type Project struct {
	ID                 ID
	Name               string
	Repo               string
	DefaultBranch      string
	DefaultWorkflow    string
	DefaultEnvironment string
	Defaults           InheritedDefaults
	RepoConfig         RepoConfig
	CreatedAt          time.Time
}
