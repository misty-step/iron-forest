// Package core declares the durable read operations every surface may perform.
// Live-run operations are deliberately absent: they need the daemon (#163).
//
// The package owns its exported data types so a surface never depends on the
// controller's internal shapes. Implementations live in the main package and
// adapt the controller's state into these types; nothing in this package
// imports the main package.
package core

// Config is the read view of forest.yaml a surface needs: the work source,
// gate checks, flow declarations, and optional projection.
type Config struct {
	Repo       string
	Checks     []Check
	Flows      Flows
	Projection ProjectionConfig
}

// Check is one gate command and its name, as declared in forest.yaml.
type Check struct {
	Name string
	Run  string
}

// Flows declares the lanes: builder, verifier, fixer, and manager. Each lane
// carries the settings that only that lane reads.
type Flows struct {
	Builder  BuilderFlowConfig
	Verifier VerifierFlowConfig
	Fixer    FixerFlowConfig
	Manager  ManagerFlowConfig
}

// FlowConfig is what every lane declares: whether it is on, which agent it
// runs, and how long it sleeps between passes.
type FlowConfig struct {
	Enabled     bool
	Agent       string
	IntervalSec int
}

// BuilderFlowConfig is the builder lane: the shared lane settings plus the
// tracker label policies that shape item selection.
type BuilderFlowConfig struct {
	FlowConfig
	ExcludeLabels []string
	RequireLabels []string
}

// VerifierFlowConfig is the verifier lane: the shared lane settings plus the
// merge policy.
type VerifierFlowConfig struct {
	FlowConfig
	Merge     string
	AutoMerge bool
}

// FixerFlowConfig is the fixer lane: the shared lane settings plus the repair
// ceiling.
type FixerFlowConfig struct {
	FlowConfig
	Attempts int
}

// ManagerFlowConfig is the manager lane: the shared lane settings plus the
// ready depth that bounds how many unstarted assignments it keeps in flight.
type ManagerFlowConfig struct {
	FlowConfig
	ReadyDepth    int
	ExcludeLabels []string
}

// ProjectionConfig is the optional, one-way human surface: publish a branch as
// a pull request and mirror decisions as comments. The factory never reads it
// back.
type ProjectionConfig struct {
	Enabled      bool
	MergeViaHost bool
}

// McpSpec declares one MCP server the agent may reach, in the read shape a
// surface lists an agent without reading the declaration file.
type McpSpec struct {
	Name    string
	Type    string
	URL     string
	Header  string
	Enabled bool
}

// AgentInfo summarizes one declared agent for a surface. Presence in the
// returned slice does not mean the declaration was readable: a directory that
// fails to load is still reported so a surface can show the failure and keep
// listing the rest, exactly as the agents command did before it used this API.
// When Err is non-empty the remaining fields carry nothing useful.
type AgentInfo struct {
	Name        string
	Description string
	CommitName  string
	CommitEmail string
	Model       string
	Variant     string
	Mode        string
	DefSHA      string
	Mcps        []McpSpec
	Err         string
}

// RunRecord is one append-only ledger row in the shape a surface reads.
type RunRecord struct {
	Time          string
	RunID         string
	Flow          string
	Subject       string
	Revision      string
	ID            string
	Branch        string
	PRURL         string
	Status        string
	TokensIn      int64
	TokensOut     int64
	CacheRead     int64
	CacheWrite    int64
	Reasoning     int64
	Agent         string
	Model         string
	BaseSHA       string
	DefSHA        string
	ReviewVerdict string
	Error         string
}

// LedgerQuery filters the read of the run ledger. A zero value reads every row,
// which is exactly what the stats command aggregates today.
type LedgerQuery struct {
	Flow string
}

// Daemon is the operator-visible state of the factory service: whether it is
// active and, when it is, how the operator can reach it.
type Daemon struct {
	Active bool
	PID    string
	Unit   string
	Note   string
}

// Verdict is the review decision note for one commit. Present reports whether a
// verdict note exists for the commit; when it is false the remaining fields are
// the zero value and are not a meaningful "absent" rendering.
type Verdict struct {
	Verdict  string
	Notes    string
	Reviewer string
	Model    string
	DefSHA   string
	RunID    string
	Time     string
	Present  bool
}

// CheckResult is one gate command and its observed result.
type CheckResult struct {
	Name    string
	Code    int
	Seconds float64
	Output  string
}

// Checks is the gate results note for one commit. Present reports whether a
// checks note exists for the commit; when it is false the remaining fields are
// the zero value and are not a meaningful "absent" rendering.
type Checks struct {
	Status  string
	Results []CheckResult
	RunID   string
	Time    string
	Present bool
}

// Comment is one tracker comment in source order.
type Comment struct {
	Body      string
	CreatedAt string
}

// Item is one tracker item and its discussion in a host-independent shape. The
// id is a string so a second work source can carry its own identity.
type Item struct {
	ID        string
	Title     string
	Body      string
	UpdatedAt string
	Tags      []string
	Comments  []Comment
}

// BranchState is one forest branch and its head commit.
type BranchState struct {
	Name string
	Head string
}

// ErrorStage names the note subsystem that failed inside a Notes call, so a
// surface reproduces the per-subsystem error prefix it used before the #176
// seam without re-deriving which read failed.
type ErrorStage string

const (
	// StageFetch marks the remote note fetch failing.
	StageFetch ErrorStage = "fetch"
	// StageVerdict marks the verdict-note read failing.
	StageVerdict ErrorStage = "verdict"
	// StageChecks marks the checks-note read failing.
	StageChecks ErrorStage = "checks"
)

// StageError wraps an error with the note subsystem that produced it. Its
// Error text is exactly the underlying error's, so a surface can prefix it
// without echoing the subsystem name twice.
type StageError struct {
	Stage ErrorStage
	Err   error
}

// Error returns the wrapped error's message unchanged.
func (e *StageError) Error() string { return e.Err.Error() }

// Unwrap exposes the wrapped error for errors.Is and errors.As.
func (e *StageError) Unwrap() error { return e.Err }

// API is every durable operation a surface may perform. Live-run operations
// are deliberately absent: they need the daemon (see #163).
type API interface {
	Config() (Config, error)
	Agents() ([]AgentInfo, error)
	Ledger(LedgerQuery) ([]RunRecord, int, error)
	Trace(runID string) ([]byte, error)
	Notes(sha string) (Verdict, Checks, error)
	Items() ([]Item, error)
	EligibleItems() ([]Item, error)
	Branches() ([]BranchState, error)
	Head() (string, error)
	Worktrees() ([]string, error)
	Daemon() (Daemon, error)
}
