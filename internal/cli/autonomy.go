package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Autonomy CLI — surfaces the per-tick evaluation audit trail added
// by S1. Before this command existed, the only way to see why a
// project wasn't scheduling tasks was grepping journald.

var (
	autonomyCmd = &cobra.Command{
		Use:   "autonomy",
		Short: "Inspect autonomy evaluation audit trail",
	}
	autonomyEvaluationsCmd = &cobra.Command{
		Use:     "evaluations",
		Aliases: []string{"evals"},
		Short:   "List recent autonomy evaluation outcomes for a project",
		Long: `Show per-tick autonomy outcomes. Each autonomy tick writes one row
regardless of whether a task was created; the outcome tells you why.

Common outcomes:
  CREATED           task successfully enqueued
  NO_ACTION         lead decided no work needed
  ACTIVE_TASKS      tick skipped because tasks were still running
  RATE_LIMITED      project or config per-minute / per-hour cap hit
  BUDGET_BLOCKED    project over its daily / monthly hard cap
  LLM_ERROR         chat provider failed
  PARSE_ERROR       LLM response unparseable as a task
  WORKFLOW_INVALID  workflow or role mismatch (common n8n-agents bug class)
  TYPE_REJECTED     LLM picked a task type outside allowedTaskTypes
  CIRCUIT_OPEN      too many recent failures — breaker tripped
  DUPLICATE         same prompt as a recent task
  COOLDOWN          same prompt failed multiple times recently
  IDEMPOTENCY_HIT   identical idempotency key already present
  DB_ERROR          task persistence layer failed
  ABORTED           eval cancelled by loop reload/shutdown (benign teardown)`,
		RunE: runAutonomyEvaluations,
	}
	autonomySummaryCmd = &cobra.Command{
		Use:   "summary",
		Short: "Aggregate autonomy outcomes by count",
		RunE:  runAutonomySummary,
	}
	autonomyHealthCmd = &cobra.Command{
		Use:   "health",
		Short: "Render the autonomy degradation table (outcome monotony, feed cadence, delivery, judge, route churn)",
		Long: `Renders the four degradation classes GET /autonomy/health computes:
outcome-mix monotony, feed cadence drift against DECLARED feeds only,
delivery resolved across the delegation edge (a CREATED tick's own status
is the router's, not the work's), and route churn.

This is the operator-facing renderer for the failure mode where every
blackbox surface reports success while the schedule, the judge, or the
router has silently degraded underneath it -- see
https://docs.vornik.io`,
		RunE: runAutonomyHealth,
	}

	autonomyProject string
	autonomyOutcome string
	autonomyLimit   int
	autonomyJSON    bool
	autonomyHours   int
)

func init() {
	autonomyEvaluationsCmd.Flags().StringVarP(&autonomyProject, "project", "p", "", "Project ID (required)")
	autonomyEvaluationsCmd.Flags().StringVar(&autonomyOutcome, "outcome", "", "Filter by outcome (e.g. CREATED, NO_ACTION, RATE_LIMITED)")
	autonomyEvaluationsCmd.Flags().IntVarP(&autonomyLimit, "limit", "n", 50, "Max rows to return (1-500)")
	autonomyEvaluationsCmd.Flags().BoolVar(&autonomyJSON, "json", false, "Output JSON instead of table")
	_ = autonomyEvaluationsCmd.MarkFlagRequired("project")

	autonomySummaryCmd.Flags().StringVarP(&autonomyProject, "project", "p", "", "Project ID (required)")
	autonomySummaryCmd.Flags().IntVar(&autonomyHours, "hours", 24, "Window length in hours (max 720)")
	autonomySummaryCmd.Flags().BoolVar(&autonomyJSON, "json", false, "Output JSON instead of table")
	_ = autonomySummaryCmd.MarkFlagRequired("project")

	autonomyHealthCmd.Flags().StringVarP(&autonomyProject, "project", "p", "", "Project ID (required)")
	autonomyHealthCmd.Flags().IntVar(&autonomyHours, "hours", 24, "Window length in hours (max 720)")
	autonomyHealthCmd.Flags().BoolVar(&autonomyJSON, "json", false, "Output JSON instead of table")
	_ = autonomyHealthCmd.MarkFlagRequired("project")

	autonomyCmd.AddCommand(autonomyEvaluationsCmd, autonomySummaryCmd, autonomyHealthCmd)
	rootCmd.AddCommand(autonomyCmd)
}

type autonomyEvalRow struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason"`
	TaskID     *string   `json:"task_id,omitempty"`
	TaskType   string    `json:"task_type"`
	WorkflowID string    `json:"workflow_id"`
	PromptHash string    `json:"prompt_hash"`
	DurationMs int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

func runAutonomyEvaluations(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	if autonomyOutcome != "" {
		q.Set("outcome", autonomyOutcome)
	}
	if autonomyLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", autonomyLimit))
	}
	path := fmt.Sprintf("/api/v1/projects/%s/autonomy/evaluations", autonomyProject)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	if autonomyJSON {
		return prettyPrintJSON(raw)
	}
	var parsed struct {
		Evaluations []autonomyEvalRow `json:"evaluations"`
		Total       int               `json:"total"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Evaluations) == 0 {
		fmt.Println("(no evaluations)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tOUTCOME\tDUR\tTYPE\tWORKFLOW\tTASK\tREASON")
	for _, e := range parsed.Evaluations {
		taskID := "—"
		if e.TaskID != nil && *e.TaskID != "" {
			taskID = shortenID(*e.TaskID)
		}
		typ := e.TaskType
		if typ == "" {
			typ = "—"
		}
		wf := e.WorkflowID
		if wf == "" {
			wf = "—"
		}
		reason := e.Reason
		if len(reason) > 80 {
			reason = reason[:77] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%dms\t%s\t%s\t%s\t%s\n",
			e.CreatedAt.Local().Format("15:04:05"),
			e.Outcome,
			e.DurationMs,
			typ,
			wf,
			taskID,
			reason,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", parsed.Total)
	return nil
}

func runAutonomySummary(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	if autonomyHours > 0 {
		q.Set("hours", fmt.Sprintf("%d", autonomyHours))
	}
	path := fmt.Sprintf("/api/v1/projects/%s/autonomy/summary", autonomyProject)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	if autonomyJSON {
		return prettyPrintJSON(raw)
	}
	var parsed struct {
		ProjectID string           `json:"projectId"`
		WindowHrs int              `json:"windowHrs"`
		Since     string           `json:"since"`
		Counts    map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Printf("Project:  %s\n", parsed.ProjectID)
	fmt.Printf("Window:   last %dh (since %s)\n", parsed.WindowHrs, parsed.Since)
	fmt.Println()

	if len(parsed.Counts) == 0 {
		fmt.Println("(no evaluations in window)")
		return nil
	}

	// Sort outcomes by count desc so the dominant outcome is first.
	type kv struct {
		name  string
		count int64
	}
	rows := make([]kv, 0, len(parsed.Counts))
	var total int64
	for k, v := range parsed.Counts {
		rows = append(rows, kv{k, v})
		total += v
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].name < rows[j].name
	})
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "OUTCOME\tCOUNT\tSHARE")
	for _, r := range rows {
		share := "—"
		if total > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.count)/float64(total))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\n", r.name, r.count, share)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", total)
	return nil
}

// autonomyHealthMaxHours mirrors the server-side cap
// (autonomyHealthMaxDeliveryRows's neighbour in internal/api/
// autonomy_handlers.go: windowHrs > 24*30 clamps to 30d = 720h). Clamping
// here too means the header reports the window that was actually
// requested, not one the server silently narrowed behind the CLI's back.
const autonomyHealthMaxHours = 720

// healthOutcomes is the wire shape of the health endpoint's "outcomes"
// block. Monotony is present in the JSON only when it fires -- Go's zero
// value (false) for an absent key is exactly the "not evidence of a
// problem" state the design calls for, so a plain bool is safe here as
// long as the renderer never prints "monotony: false" for it (see
// renderAutonomyHealth).
type healthOutcomes struct {
	Counts   map[string]int64 `json:"counts"`
	Total    int64            `json:"total"`
	Monotony bool             `json:"monotony"`
}

// healthFeedRow mirrors autonomyFeedJSON (internal/api/autonomy_handlers.go).
type healthFeedRow struct {
	Slug           string  `json:"slug"`
	CadenceSeconds float64 `json:"cadenceSeconds"`
	LagSeconds     float64 `json:"lagSeconds"`
	NeverRan       bool    `json:"neverRan"`
	// Unmeasured means "no run inside the examined history", as opposed
	// to "this feed has never run". The API bounds that history at a
	// page of tasks; a feed whose cadence is longer than the page spans
	// sits outside it most of its cycle, and printing that as "never
	// ran" reports "examined and clean" while meaning "never examined".
	Unmeasured bool `json:"unmeasured"`
	// LagAtLeastSeconds is a lower BOUND for an unmeasured feed, never a
	// measurement -- rendered with a leading ">" for exactly that reason.
	LagAtLeastSeconds float64 `json:"lagAtLeastSeconds"`
	HorizonTasks      int     `json:"horizonTasks"`
	HorizonOldest     string  `json:"horizonOldest"`
	Breach            string  `json:"breach"`
}

// healthDeliveryRow mirrors autonomyDeliveryRow (internal/api/
// autonomy_handlers.go). ChildTaskID is omitted (nil), not present-but-
// empty, when the tick did not delegate.
type healthDeliveryRow struct {
	TaskID       string  `json:"taskId"`
	ChildTaskID  *string `json:"childTaskId,omitempty"`
	Status       string  `json:"status"`
	NoDelegation bool    `json:"noDelegation"`
}

type healthDelivery struct {
	Rows      []healthDeliveryRow `json:"rows"`
	Truncated bool                `json:"truncated"`
	// Unresolved counts failed LOOKUPS (task/children/verdict/execution),
	// not ticks -- one tick can fail two lookups and count twice here.
	// Never render this as a tick count.
	Unresolved int64 `json:"unresolved"`
}

// judgeBlock mirrors buildAutonomyJudgeBlock's two shapes: {declared:
// false} when there is no verdict repo wired or zero verdicts resolved in
// the window, or a populated block otherwise. Declared is the only field
// that always decodes; the rest are the zero value when declared is
// false, which the renderer must never print as a real 0% failure rate.
type judgeBlock struct {
	Declared bool             `json:"declared"`
	Counts   map[string]int64 `json:"counts"`
	Total    int64            `json:"total"`
	FailRate float64          `json:"failRate"`
}

// healthPayload is the CLI's view of the Task 5 JSON contract
// (GET /api/v1/projects/{id}/autonomy/health). RouteChurn is deliberately
// a map, not a struct: its shape is NOT uniform across code paths --
// an empty map{} on the nil-repo path, versus five named keys when
// populated -- so the renderer must check key presence rather than
// assume the shape (design 2026-09-10-autonomy-degradation-detection-
// design.md §6, Task 5 implementation).
type healthPayload struct {
	ProjectID     string          `json:"projectId"`
	WindowHrs     int             `json:"windowHrs"`
	Since         string          `json:"since"`
	Outcomes      healthOutcomes  `json:"outcomes"`
	FeedsDeclared bool            `json:"feedsDeclared"`
	Feeds         []healthFeedRow `json:"feeds"`
	Delivery      healthDelivery  `json:"delivery"`
	Judge         judgeBlock      `json:"judge"`
	RouteChurn    map[string]any  `json:"routeChurn"`
}

func runAutonomyHealth(_ *cobra.Command, _ []string) error {
	hours := autonomyHours
	if hours <= 0 {
		hours = 24
	}
	if hours > autonomyHealthMaxHours {
		hours = autonomyHealthMaxHours
	}
	q := url.Values{}
	q.Set("hours", fmt.Sprintf("%d", hours))
	path := fmt.Sprintf("/api/v1/projects/%s/autonomy/health?%s", autonomyProject, q.Encode())
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	if autonomyJSON {
		return prettyPrintJSON(raw)
	}
	var p healthPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Print(renderAutonomyHealth(p))
	return nil
}

// autonomyHealthRouteChurnFields is the fixed set of named keys the
// populated routeChurn shape carries (design §6 correction, 2026-09-10).
// Order matters here: it is the render order.
var autonomyHealthRouteChurnFields = []struct {
	key   string
	label string
}{
	{"tasksObserved", "tasks observed"},
	{"tasksWithExecution", "tasks with execution"},
	{"executionsPerTask", "executions per task"},
	{"maxExecutionsForTask", "max executions for one task"},
	{"orphanedStepOutcomes", "orphaned step outcomes"},
}

// renderAutonomyHealth is a pure function over the decoded payload so the
// honesty strings ("not declared", "no verdicts", "not measured", "never
// ran", "no delegation") are unit-testable without a daemon. runAutonomyHealth
// is the thin fetch-and-print wrapper; keep it that way -- runAutonomySummary
// mixes fetch and render and is not the shape to copy.
func renderAutonomyHealth(p healthPayload) string {
	var buf bytes.Buffer

	since := p.Since
	if since == "" {
		since = "n/a"
	}
	fmt.Fprintf(&buf, "Project:  %s\n", p.ProjectID)
	fmt.Fprintf(&buf, "Window:   last %dh (since %s)\n", p.WindowHrs, since)
	fmt.Fprintf(&buf, "Ticks:    %d evaluations in window\n", p.Outcomes.Total)
	fmt.Fprintln(&buf, "Note:     cadence adherence below covers DECLARED feeds only -- it says")
	fmt.Fprintln(&buf, "          nothing about a feed the goal mentions but `feeds` omits.")
	buf.WriteString("\n")

	renderHealthOutcomes(&buf, p.Outcomes)
	buf.WriteString("\n")
	renderHealthCadence(&buf, p)
	buf.WriteString("\n")
	renderHealthDelivery(&buf, p.Delivery)
	buf.WriteString("\n")
	renderHealthJudge(&buf, p.Judge)
	buf.WriteString("\n")
	renderHealthRouteChurn(&buf, p.RouteChurn)

	return buf.String()
}

func renderHealthOutcomes(buf *bytes.Buffer, o healthOutcomes) {
	buf.WriteString("OUTCOMES\n")
	if len(o.Counts) == 0 {
		buf.WriteString("  (no evaluations in window)\n")
		return
	}
	type kv struct {
		name  string
		count int64
	}
	rows := make([]kv, 0, len(o.Counts))
	for k, v := range o.Counts {
		rows = append(rows, kv{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].name < rows[j].name
	})
	tw := tabwriter.NewWriter(buf, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  OUTCOME\tCOUNT\tSHARE")
	for _, r := range rows {
		share := "—"
		if o.Total > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.count)/float64(o.Total))
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%d\t%s\n", r.name, r.count, share)
	}
	_ = tw.Flush()
	// Monotony is present in the JSON only when it fires; absence is the
	// normal state and is never rendered as "monotony: false".
	if o.Monotony {
		buf.WriteString("  ! monotony: one outcome held >=95% of ticks this window\n")
	}
}

// renderHealthCadence is the cadence honesty rule: a project with no
// declared feeds prints "not declared", never OK and never a blank that
// reads as OK.
func renderHealthCadence(buf *bytes.Buffer, p healthPayload) {
	buf.WriteString("CADENCE (declared feeds only)\n")
	if !p.FeedsDeclared {
		buf.WriteString("  not declared\n")
		return
	}
	if len(p.Feeds) == 0 {
		buf.WriteString("  declared, no feed observations in window\n")
		return
	}
	tw := tabwriter.NewWriter(buf, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  SLUG\tCADENCE\tLAG\tBREACH")
	var unmeasured []healthFeedRow
	for _, f := range p.Feeds {
		cadence := time.Duration(f.CadenceSeconds * float64(time.Second)).Round(time.Second)
		lag := healthFeedLag(f)
		if f.Unmeasured {
			unmeasured = append(unmeasured, f)
		}
		breach := f.Breach
		if breach == "" {
			breach = "-"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", f.Slug, cadence, lag, breach)
	}
	_ = tw.Flush()
	renderHealthCadenceHorizon(buf, unmeasured)
}

// healthFeedLag renders the LAG cell. Three distinct states, never
// collapsed into one another:
//
//	measured    "8h0m0s"     -- an observed age
//	unmeasured  ">233h0m0s"  -- a lower BOUND; older history not examined
//	never ran   "never ran"  -- the whole history was examined, no run
//
// The middle state is the one that did not exist before 2026-09-11: a
// feed 200 days overdue printed "never ran", byte-identical to one that
// had genuinely never run.
func healthFeedLag(f healthFeedRow) string {
	if !f.NeverRan {
		return time.Duration(f.LagSeconds * float64(time.Second)).Round(time.Second).String()
	}
	if f.Unmeasured {
		if f.LagAtLeastSeconds > 0 {
			return ">" + time.Duration(f.LagAtLeastSeconds*float64(time.Second)).Round(time.Second).String()
		}
		return "not measured"
	}
	return "never ran"
}

// renderHealthCadenceHorizon publishes the scope of an unmeasured row's
// claim: which feeds it covers, how much history was examined, and how
// far back it reached. A number with no scope reads as a guarantee
// (project rule 4), and ">233h" on its own does not say why.
func renderHealthCadenceHorizon(buf *bytes.Buffer, unmeasured []healthFeedRow) {
	if len(unmeasured) == 0 {
		return
	}
	slugs := make([]string, 0, len(unmeasured))
	for _, f := range unmeasured {
		slugs = append(slugs, f.Slug)
	}
	sort.Strings(slugs)
	since := unmeasured[0].HorizonOldest
	if since == "" {
		since = "n/a"
	}
	fmt.Fprintf(buf, "  ! not measured (%s): no run inside the examined history --\n", strings.Join(slugs, ", "))
	fmt.Fprintf(buf, "    the last %d tasks, back to %s. A \">\" lag is a lower bound,\n",
		unmeasured[0].HorizonTasks, since)
	buf.WriteString("    not a measurement: older history was not looked at.\n")
}

// renderHealthDelivery reports what each CREATED tick actually delivered,
// following the delegation edge, and separately flags the caveat on
// "unresolved" -- it counts failed lookups, not ticks, so it is never
// labelled as a tick count.
func renderHealthDelivery(buf *bytes.Buffer, d healthDelivery) {
	buf.WriteString("DELIVERY\n")
	if len(d.Rows) == 0 {
		buf.WriteString("  (no CREATED ticks resolved in window)\n")
	} else {
		tw := tabwriter.NewWriter(buf, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  TASK\tCHILD\tSTATUS\tDELEGATION")
		for _, r := range d.Rows {
			child := "-"
			if r.ChildTaskID != nil && *r.ChildTaskID != "" {
				child = shortenID(*r.ChildTaskID)
			}
			delegation := "delegated"
			if r.NoDelegation {
				delegation = "no delegation"
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", shortenID(r.TaskID), child, r.Status, delegation)
		}
		_ = tw.Flush()
	}
	if d.Truncated {
		buf.WriteString("  truncated: more CREATED ticks exist in this window than were resolved\n")
	}
	if d.Unresolved > 0 {
		// Deliberately "lookups", not "ticks", in BOTH the label and the
		// parenthetical: one tick can fail two lookups (verdict AND
		// execution, on the same already-resolved task -- see
		// tallyDelivery in internal/api/autonomy_handlers.go) and count
		// twice here. A tick-flavoured word anywhere in this line reads
		// as "N ticks were unresolvable", which is exactly the claim
		// this number cannot support.
		fmt.Fprintf(buf, "  unresolved lookups: %d (task/children/verdict/execution lookups that failed this pass)\n", d.Unresolved)
	}
}

// renderHealthJudge is the judge half of the honesty rule: an undeclared
// or empty verdict window prints "no verdicts", never a fabricated 0%
// failure rate.
func renderHealthJudge(buf *bytes.Buffer, j judgeBlock) {
	buf.WriteString("JUDGE\n")
	if !j.Declared {
		buf.WriteString("  no verdicts\n")
		return
	}
	fmt.Fprintf(buf, "  total=%d failRate=%.1f%%\n", j.Total, j.FailRate*100)
	if len(j.Counts) == 0 {
		return
	}
	names := make([]string, 0, len(j.Counts))
	for k := range j.Counts {
		names = append(names, k)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(buf, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  VERDICT\tCOUNT")
	for _, n := range names {
		_, _ = fmt.Fprintf(tw, "  %s\t%d\n", n, j.Counts[n])
	}
	_ = tw.Flush()
}

// renderHealthRouteChurn checks key PRESENCE rather than assuming the
// shape -- routeChurn is deliberately non-uniform ({} on the nil-repo
// path, five named keys when populated). A missing key renders "not
// measured", never a silent 0.
func renderHealthRouteChurn(buf *bytes.Buffer, churn map[string]any) {
	buf.WriteString("ROUTE CHURN\n")
	tw := tabwriter.NewWriter(buf, 2, 4, 2, ' ', 0)
	for _, f := range autonomyHealthRouteChurnFields {
		v, ok := churn[f.key]
		if !ok {
			_, _ = fmt.Fprintf(tw, "  %s\tnot measured\n", f.label)
			continue
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%v\n", f.label, v)
	}
	_ = tw.Flush()
}

// shortenID returns the tail-hex chunk of a vornik ID so table rows
// stay readable (task_20260423233059_abcd1234 → …abcd1234).
func shortenID(id string) string {
	// Vornik IDs are {prefix}_{timestamp}_{hex} — keep just the hex.
	// Fall back to the last 8 chars when the shape is unexpected.
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '_' {
			return id[i+1:]
		}
	}
	if len(id) > 12 {
		return id[len(id)-12:]
	}
	return id
}
