package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	braketsdk "github.com/aws/aws-sdk-go-v2/service/braket"
	"github.com/aws/aws-sdk-go-v2/service/braket/types"
	"github.com/spf13/cobra"

	"github.com/spore-host/puffball/internal/scrub"
	pbraket "github.com/spore-host/puffball/pkg/braket"
	"github.com/spore-host/puffball/pkg/gate"
)

// newCancellers is the cancel client factory; tests replace it.
var newCancellers = gate.AWSCancellers

// shortDevice is a device ARN's last path element (Cepheus-1-108Q, sv1).
func shortDevice(arn string) string { return taskID(arn) }

// display scrubs account IDs from service text and shows them as <account>
// (law 7: output gets pasted).
func display(s string) string {
	return strings.ReplaceAll(scrub.Error(errors.New(s)).Error(), "000000000000", "<account>")
}

// taskView is a task as puff shows it. TaskArn has the account replaced by
// <account> (law 7), so it is for reading, not for pasting into other tools;
// TaskID and Region identify the task, and puff's commands take TaskID.
type taskView struct {
	TaskID       string            `json:"task_id"`
	TaskArn      string            `json:"task_arn"`
	Region       string            `json:"region"`
	Device       string            `json:"device"`
	DeviceArn    string            `json:"device_arn"`
	Status       string            `json:"status"`
	Shots        int64             `json:"shots"`
	CreatedAt    *time.Time        `json:"created_at,omitempty"`
	EndedAt      *time.Time        `json:"ended_at,omitempty"`
	Project      string            `json:"project,omitempty"`
	RunID        string            `json:"run_id,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
	Queue        string            `json:"queue,omitempty"`
	QueuePos     string            `json:"queue_position,omitempty"`
	QueueMessage string            `json:"queue_message,omitempty"`
	Failure      string            `json:"failure_reason,omitempty"`
	ActionType   string            `json:"action_type,omitempty"`
	Programs     int64             `json:"programs,omitempty"`
	Results      string            `json:"results,omitempty"`
}

func viewARN(region, id string) string { return pbraket.TaskARN(region, "<account>", id) }

func viewSummary(t pbraket.Task) taskView {
	s := t.Summary
	id := taskID(aws.ToString(s.QuantumTaskArn))
	return taskView{TaskID: id, TaskArn: viewARN(t.Region, id), Region: t.Region,
		Device: shortDevice(aws.ToString(s.DeviceArn)), DeviceArn: aws.ToString(s.DeviceArn),
		Status: string(s.Status), Shots: aws.ToInt64(s.Shots), CreatedAt: s.CreatedAt, EndedAt: s.EndedAt,
		Project: s.Tags[tagProject], RunID: s.Tags[tagRunID], Tags: s.Tags}
}

func viewTask(g *braketsdk.GetQuantumTaskOutput) taskView {
	arn := aws.ToString(g.QuantumTaskArn)
	region, id := pbraket.RegionOf(arn), taskID(arn)
	v := taskView{TaskID: id, TaskArn: viewARN(region, id), Region: region,
		Device: shortDevice(aws.ToString(g.DeviceArn)), DeviceArn: aws.ToString(g.DeviceArn),
		Status: string(g.Status), Shots: aws.ToInt64(g.Shots), CreatedAt: g.CreatedAt, EndedAt: g.EndedAt,
		Project: g.Tags[tagProject], RunID: g.Tags[tagRunID], Tags: g.Tags,
		Failure: display(aws.ToString(g.FailureReason))}
	if q := g.QueueInfo; q != nil {
		v.Queue, v.QueuePos, v.QueueMessage = string(q.Queue), aws.ToString(q.Position), aws.ToString(q.Message)
	}
	if m := g.ActionMetadata; m != nil {
		v.ActionType, v.Programs = aws.ToString(m.ActionType), aws.ToInt64(m.ProgramCount)
	}
	if b, d := aws.ToString(g.OutputS3Bucket), aws.ToString(g.OutputS3Directory); b != "" {
		v.Results = display("s3://" + b + "/" + d + "/")
	}
	return v
}

// resolveTask reads a task given its ARN, or its ID (then every region is
// asked, using the account from STS to build the ARN).
func resolveTask(ctx context.Context, ref string) (*braketsdk.GetQuantumTaskOutput, error) {
	f, err := newFactory(ctx)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(ref, "arn:") {
		if !strings.Contains(ref, ":quantum-task/") || pbraket.RegionOf(ref) == "" || !pbraket.TaskIDPattern.MatchString(taskID(ref)) {
			return nil, usageErrorf("%q is not a quantum task ARN", ref)
		}
		g, err := pbraket.GetTask(ctx, f, ref)
		if err != nil {
			return nil, scrub.Error(err)
		}
		return g, nil
	}
	if !pbraket.TaskIDPattern.MatchString(ref) {
		return nil, usageErrorf("%q is not a task ID (a UUID) or a task ARN", ref)
	}
	acct, err := newResults().Account(ctx)
	if err != nil {
		return nil, scrub.Error(fmt.Errorf("account, to build the task ARN: %w", err))
	}
	// The same regions ls covers: the SSM list plus any a device ARN names.
	inv, derr := pbraket.Discover(ctx, f)
	if inv == nil {
		return nil, derr
	}
	g, err := pbraket.FindTask(ctx, f, inv.Regions(), acct, ref)
	if err == nil {
		return g, nil
	}
	if _, ok := pbraket.AsIncomplete(derr); ok {
		// Not found, and discovery couldn't search everywhere: the task may
		// be in a region that failed, so name it.
		if _, ok := pbraket.AsIncomplete(err); !ok {
			err = nil
		}
		return nil, mergeIncomplete("find task "+ref, derr, err)
	}
	return nil, err
}

func ctxOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeTaskDetail(w io.Writer, v taskView) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { _, _ = fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	row("task", v.TaskID)
	row("region", v.Region)
	row("device", v.Device)
	row("status", v.Status)
	if v.QueuePos != "" && v.QueuePos != "None" {
		row("queue", v.Queue+" position "+v.QueuePos)
	}
	if v.Failure != "" {
		row("failure", v.Failure)
	}
	row("shots", fmt.Sprint(v.Shots))
	if v.Programs > 0 {
		row("programs", fmt.Sprintf("%d (program set)", v.Programs))
	}
	row("created", when(v.CreatedAt))
	row("ended", when(v.EndedAt))
	row("project", dash(v.Project))
	row("run id", dash(v.RunID))
	if v.Results != "" {
		row("results", v.Results)
	}
	_ = tw.Flush()
}

// --- ls ---

var lsFlags struct {
	states  []string
	since   time.Duration
	project string
	device  string
	mine    bool
}

var lsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List quantum tasks in every Braket region",
	Long: `List quantum tasks in every Braket region, whoever submitted them: puff,
the Braket SDK, a notebook, a colleague. Untagged tasks show "-" for project
and run id.

Regions come from the public SSM Braket region list, plus any region a device
ARN names. A region that fails is named, and the command exits 1 after
listing the rest.`,
	Example: `  puff ls
  puff ls --state queued --state running
  puff ls --since 24h --project fmo --json`,
	Args: usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := ctxOf(cmd)
		q := pbraket.TaskQuery{}
		for _, s := range lsFlags.states {
			st := types.QuantumTaskStatus(strings.ToUpper(s))
			if !knownStatus(st) {
				return usageErrorf("--state %q: want one of created, queued, running, cancelling, completed, failed, cancelled", s)
			}
			q.Statuses = append(q.Statuses, st)
		}
		if lsFlags.since < 0 {
			return usageErrorf("--since must not be negative")
		}
		if lsFlags.since > 0 {
			q.Since = clockNow().Add(-lsFlags.since)
		}
		if lsFlags.project != "" {
			q.Tags = map[string]string{tagProject: lsFlags.project}
		}
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		tasks, err := listTasks(ctx, f, q)
		if tasks == nil && err != nil {
			return err
		}
		var views []taskView
		for _, t := range tasks {
			if lsFlags.mine && t.Summary.Tags[tagRunID] == "" {
				continue
			}
			if lsFlags.device != "" && !strings.EqualFold(shortDevice(aws.ToString(t.Summary.DeviceArn)), lsFlags.device) {
				continue
			}
			views = append(views, viewSummary(t))
		}
		if jsonOutput {
			out := struct {
				Tasks         []taskView        `json:"tasks"`
				Complete      bool              `json:"complete"`
				RegionsFailed map[string]string `json:"regions_failed"`
			}{Tasks: views, Complete: err == nil, RegionsFailed: failedRegions(err)}
			if out.Tasks == nil {
				out.Tasks = []taskView{}
			}
			if werr := writeJSON(cmd.OutOrStdout(), out); werr != nil {
				return werr
			}
			return err
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "TASK\tREGION\tDEVICE\tSTATUS\tSHOTS\tCREATED\tPROJECT\tRUN ID")
		for _, v := range views {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", v.TaskID, v.Region, v.Device, v.Status, v.Shots, when(v.CreatedAt), dash(v.Project), dash(v.RunID))
		}
		_ = tw.Flush()
		if len(views) == 0 {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "no tasks match")
		}
		return err
	},
}

func knownStatus(s types.QuantumTaskStatus) bool {
	for _, v := range types.QuantumTaskStatus("").Values() {
		if s == v {
			return true
		}
	}
	return false
}

// listTasks searches every region discovery covers. A partial result comes
// back with the *IncompleteError naming what's missing.
func listTasks(ctx context.Context, f pbraket.Factory, q pbraket.TaskQuery) ([]pbraket.Task, error) {
	inv, derr := pbraket.Discover(ctx, f)
	if inv == nil {
		return nil, derr
	}
	tasks, serr := pbraket.SearchTasks(ctx, f, inv.Regions(), q)
	if tasks == nil {
		tasks = []pbraket.Task{}
	}
	return tasks, mergeIncomplete("list tasks", derr, serr)
}

// mergeIncomplete combines per-region failures from several steps into one
// *IncompleteError; any other error wins as is.
func mergeIncomplete(op string, errs ...error) error {
	failed := map[string]error{}
	for _, err := range errs {
		if err == nil {
			continue
		}
		ie, ok := pbraket.AsIncomplete(err)
		if !ok {
			return err
		}
		for r, e := range ie.Failed {
			failed[r] = e
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &pbraket.IncompleteError{Op: op, Failed: failed}
}

func failedRegions(err error) map[string]string {
	out := map[string]string{}
	if ie, ok := pbraket.AsIncomplete(err); ok {
		for r, e := range ie.Failed {
			out[r] = display(e.Error())
		}
	}
	return out
}

// --- get ---

var getCmd = &cobra.Command{
	Use:   "get <task-id|task-arn>",
	Short: "Show one quantum task, with its queue position",
	Long: `Show one quantum task: status, queue position while it waits, device,
shots, times, tags and where its results are. Given a task ID (the UUID at
the end of its ARN), every Braket region is asked.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		g, err := resolveTask(ctxOf(cmd), args[0])
		if err != nil {
			return err
		}
		v := viewTask(g)
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), v)
		}
		writeTaskDetail(cmd.OutOrStdout(), v)
		return nil
	},
}

// --- wait ---

var waitFlags struct {
	timeout  time.Duration
	interval time.Duration
}

// waitSleep pauses between polls; tests replace it.
var waitSleep = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

var waitCmd = &cobra.Command{
	Use:   "wait <task-id|task-arn>",
	Short: "Wait for a quantum task to finish",
	Long: `Poll a quantum task until it is COMPLETED, FAILED or CANCELLED, printing each
change of status and queue position to stderr. Exits 0 when the task
completes, 1 when it fails, is cancelled, or is still running at --timeout.
Ctrl-C stops waiting; it does not cancel the task.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		if waitFlags.interval <= 0 || waitFlags.timeout <= 0 {
			return usageErrorf("--interval and --timeout must be positive")
		}
		ctx := ctxOf(cmd)
		g, err := resolveTask(ctx, args[0])
		if err != nil {
			return err
		}
		arn := aws.ToString(g.QuantumTaskArn)
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		deadline := clockNow().Add(waitFlags.timeout)
		last, fails := "", 0
		for {
			v := viewTask(g)
			if state := v.Status + " " + v.QueuePos; state != last {
				line := v.Status
				if v.QueuePos != "" && v.QueuePos != "None" {
					line += ", queue position " + v.QueuePos
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s %s: %s\n", clockNow().UTC().Format("15:04:05Z"), v.TaskID, line)
				last = state
			}
			if pbraket.Terminal(g.Status) {
				if jsonOutput {
					if err := writeJSON(cmd.OutOrStdout(), v); err != nil {
						return err
					}
				} else {
					writeTaskDetail(cmd.OutOrStdout(), v)
				}
				if g.Status != types.QuantumTaskStatusCompleted {
					return fmt.Errorf("task %s ended %s%s", v.TaskID, v.Status, suffix(v.Failure))
				}
				return nil
			}
			stopped := func(err error) error {
				if jsonOutput {
					if werr := writeJSON(cmd.OutOrStdout(), v); werr != nil {
						return werr
					}
				}
				return err
			}
			left := deadline.Sub(clockNow())
			if left <= 0 {
				return stopped(fmt.Errorf("task %s is still %s after %s", v.TaskID, v.Status, waitFlags.timeout))
			}
			if err := waitSleep(ctx, min(waitFlags.interval, left)); err != nil {
				return stopped(fmt.Errorf("stopped waiting for %s (the task is still %s; it was not cancelled): %w", v.TaskID, v.Status, err))
			}
			next, err := pbraket.GetTask(ctx, f, arn)
			switch {
			case err != nil && ctx.Err() != nil:
				return stopped(fmt.Errorf("stopped waiting for %s (the task is still %s; it was not cancelled): %w", v.TaskID, v.Status, ctx.Err()))
			case err != nil:
				// A throttle or network blip outlasting the SDK's retries
				// doesn't end the wait; repeated failures do.
				fails++
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: polling %s failed (%d of %d): %v\n", v.TaskID, fails, maxPollFailures, scrub.Error(err))
				if fails >= maxPollFailures {
					return stopped(fmt.Errorf("stopped waiting for %s after %d failed polls (the task was %s; it was not cancelled): %w", v.TaskID, fails, v.Status, scrub.Error(err)))
				}
			default:
				g, fails = next, 0
			}
		}
	},
}

// maxPollFailures is how many polls in a row wait lets fail before giving up.
const maxPollFailures = 3

func suffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// --- cancel ---

var cancelCmd = &cobra.Command{
	Use:   "cancel <task-id|task-arn>",
	Short: "Cancel a quantum task",
	Long: `Cancel a quantum task that hasn't finished. Cancelling isn't billed (it isn't
one of puff's billable operations) but it changes the task for good, so puff
reads the task first and says what it is cancelling. A task that has already
finished is left alone.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := ctxOf(cmd)
		g, err := resolveTask(ctx, args[0])
		if err != nil {
			return err
		}
		f, err := newFactory(ctx)
		if err != nil {
			return err
		}
		v := viewTask(g)
		if pbraket.Terminal(g.Status) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "task %s is already %s; nothing to cancel\n", v.TaskID, v.Status)
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"task_id": v.TaskID, "region": v.Region, "status": v.Status, "cancelled": false})
			}
			return nil
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cancelling task %s on %s in %s (%s, project %s)\n", v.TaskID, v.Device, v.Region, v.Status, dash(v.Project))
		status, err := gate.Cancel(ctx, newCancellers(), v.Region, aws.ToString(g.QuantumTaskArn))
		if err != nil {
			// The task may have finished between the read and the cancel.
			if now, gerr := pbraket.GetTask(ctx, f, aws.ToString(g.QuantumTaskArn)); gerr == nil && pbraket.Terminal(now.Status) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "task %s finished (%s) before it could be cancelled: %v\n", v.TaskID, now.Status, err)
				if jsonOutput {
					return writeJSON(cmd.OutOrStdout(), map[string]any{"task_id": v.TaskID, "region": v.Region, "status": string(now.Status), "cancelled": false})
				}
				return nil
			}
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), map[string]any{"task_id": v.TaskID, "region": v.Region, "status": string(status), "cancelled": true})
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "task %s: %s\n", v.TaskID, status)
		return nil
	},
}

func init() {
	lsCmd.Flags().StringArrayVar(&lsFlags.states, "state", nil, "Only tasks in this state (repeatable): created, queued, running, cancelling, completed, failed, cancelled")
	lsCmd.Flags().DurationVar(&lsFlags.since, "since", 30*24*time.Hour, "Only tasks created within this long (0: all)")
	lsCmd.Flags().StringVar(&lsFlags.project, "project", "", "Only tasks with this puffball:project tag")
	lsCmd.Flags().StringVar(&lsFlags.device, "device", "", "Only tasks on this device (as shown in the DEVICE column)")
	lsCmd.Flags().BoolVar(&lsFlags.mine, "mine", false, "Only tasks puff submitted (those with a puffball:run-id tag)")
	waitCmd.Flags().DurationVar(&waitFlags.timeout, "timeout", 30*time.Minute, "Stop waiting after this long")
	waitCmd.Flags().DurationVar(&waitFlags.interval, "interval", 10*time.Second, "Time between polls")
	rootCmd.AddCommand(lsCmd, getCmd, waitCmd, cancelCmd)
}
