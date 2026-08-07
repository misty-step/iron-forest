// Package core declares the durable read operations every surface may perform.
// Live-run operations are deliberately absent: they need the daemon (#163).
//
// The package owns its exported data types so a surface never depends on the
// controller's internal shapes. Implementations live in the main package and
// adapt the controller's state into these types; nothing in this package
// imports the main package.
package core

// CommitIdentity is the author every flow's commits carry. It is declared, not
// derived from a host account, so a run is attributable in any repository.
type CommitIdentity struct {
	Name  string
	Email string
}

// Config is the read view of forest.yaml a surface needs: the work source, the
// gate checks, the flow declarations, the commit identity, and the optional
// projection.
type Config struct {
	Repo       string
	Commit     CommitIdentity
	Checks     []Check
	Flows      Flows
	Projection ProjectionConfig
}

// Check is one gate command and its name, as declared in forest.yaml.
type Check struct {
	Name string
	Run  string
}

// Flows declares the lanes: builder, verifier, and fixer. Each lane carries the
// settings that only that lane reads.
type Flows struct {
	Builder  BuilderFlowConfig
	Verifier VerifierFlowConfig
	Fixer    FixerFlowConfig
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

// AgentInfo summarizes one declared agent for a surface.
type AgentInfo struct {
	Name        string
	Description string
	Model       string
	Variant     string
	Mode        string
	DefSHA      string
	Mcps        []McpSpec
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

// Verdict is the review decision note for one commit.
type Verdict struct {
	Verdict  string
	Notes    string
	Reviewer string
	Model    string
	DefSHA   string
	RunID    string
	Time     string
}

// CheckResult is one gate command and its observed result.
type CheckResult struct {
	Name    string
	Code    int
	Seconds float64
	Output  string
}

// Checks is the gate results note for one commit.
type Checks struct {
	Status  string
	Results []CheckResult
	RunID   string
	Time    string
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

// API is every durable operation a surface may perform. Live-run operations
// are deliberately absent: they need the daemon (see #163).
type API interface {
	Config() (Config, error)
	Agents() ([]AgentInfo, error)
	Ledger(LedgerQuery) ([]RunRecord, error)
	Trace(runID string) ([]byte, error)
	Notes(sha string) (Verdict, Checks, error)
	Items() ([]Item, error)
	Branches() ([]BranchState, error)
	DaemonPresent() (bool, error)
}
