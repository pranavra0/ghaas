package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pranavra0/ghaas/internal/github"
	"github.com/pranavra0/ghaas/internal/state"
)

func (r *Root) client() (github.GitHub, error) {
	if r.services.GitHub != nil {
		return r.services.GitHub, nil
	}
	if r.services.NewGitHub == nil {
		return nil, errors.New("GitHub client is not configured")
	}
	client, err := r.services.NewGitHub()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("GitHub constructor returned nil client")
	}
	return client, nil
}

func (r *Root) invoke(ctx context.Context, args []string) error {
	fs := newFlags("invoke")
	ref := fs.String("ref", r.services.DefaultRef, "git ref")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid invoke options")
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas invoke [--ref REF] FUNCTION")
	}
	name := fs.Arg(0)
	if strings.TrimSpace(name) == "" {
		return errors.New("function name is required")
	}
	m, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := selectedFunctions(m, name); err != nil {
		return err
	}
	uuid, err := newUUID()
	if err != nil {
		return err
	}
	logicalID := name + "/" + uuid
	maxAttempts := 1
	if fn, ok := m.Functions[name]; ok {
		maxAttempts = fn.EffectiveMaxAttempts(m.Defaults)
		if maxAttempts < 1 {
			maxAttempts = 1
		}
	}
	// Ensure is deliberately before dispatch. If dispatch returns an unknown
	// error, the durable pending record remains the source of truth and a
	// caller can inspect it without creating a replacement logical ID.
	if r.services.State != nil {
		if _, err := r.services.State.Ensure(ctx, name, logicalID, maxAttempts); err != nil {
			return err
		}
	}
	client, err := r.client()
	if err != nil {
		return err
	}
	fmt.Fprintf(r.services.Stdout, "◆ %s  pending\n  %s\n", name, logicalID)
	if err := client.DispatchWorkflow(ctx, r.services.WorkflowFor(name), *ref, map[string]string{"ghaas_invocation_id": uuid}); err != nil {
		fmt.Fprintf(r.services.Stdout, "  dispatch uncertain; inspect with `ghaas status %s --invocation %s`\n", name, logicalID)
		return fmt.Errorf("dispatch outcome uncertain for %s (inspect with `ghaas status %s --invocation %s`): %w", logicalID, name, logicalID, err)
	}
	return nil
}

func newUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate invocation UUID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return hex.EncodeToString(raw[:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" + hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:]), nil
}

func latestRun(runs []github.WorkflowRun) (github.WorkflowRun, bool) {
	if len(runs) == 0 {
		return github.WorkflowRun{}, false
	}
	latest := runs[0]
	for _, run := range runs[1:] {
		if run.CreatedAt.After(latest.CreatedAt) || (run.CreatedAt.Equal(latest.CreatedAt) && run.ID > latest.ID) {
			latest = run
		}
	}
	return latest, true
}

func exactRun(runs []github.WorkflowRun, logicalID string) (github.WorkflowRun, bool) {
	run, ok, _ := matchExactRun(runs, logicalID)
	return run, ok
}

func matchExactRun(runs []github.WorkflowRun, logicalID string) (github.WorkflowRun, bool, bool) {
	logicalID = strings.TrimSpace(logicalID)
	if logicalID == "" {
		return github.WorkflowRun{}, false, false
	}
	title := "ghaas: " + logicalID
	var match github.WorkflowRun
	found := false
	for _, run := range runs {
		// Explicit lookup is intentionally stricter than the default latest
		// lookup: a logical ID can only identify its canonical display title.
		if run.DisplayTitle != title {
			continue
		}
		if !found {
			match, found = run, true
			continue
		}
		if run.ID != match.ID {
			return github.WorkflowRun{}, false, true
		}
	}
	return match, found, false
}

func canonicalDisplayTitle(logicalID string) string {
	return "ghaas: " + strings.TrimSpace(logicalID)
}

func validateLogicalID(function, id string) error {
	if !strings.HasPrefix(id, function+"/") || strings.TrimPrefix(id, function+"/") == "" {
		return fmt.Errorf("invocation %q does not belong to function %q", id, function)
	}
	return nil
}

type displayTitleLister interface {
	ListWorkflowRunsByDisplayTitle(context.Context, string, string) ([]github.WorkflowRun, error)
}

type attemptLogsGetter interface {
	GetWorkflowAttemptLogs(context.Context, int64, int) (io.ReadCloser, error)
}

type invocationLister interface {
	List(context.Context, string, int) ([]state.Invocation, error)
}

func (r *Root) status(ctx context.Context, args []string) error {
	fs := newFlags("status")
	invocationID := fs.String("invocation", "", "exact logical invocation ID")
	jsonOutput := fs.Bool("json", false, "print stable JSON")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid status options")
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas status FUNCTION [--invocation ID] [--json]")
	}
	name := fs.Arg(0)
	m, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := selectedFunctions(m, name); err != nil {
		return err
	}
	target := strings.TrimSpace(*invocationID)
	if target != "" {
		if err := validateLogicalID(name, target); err != nil {
			return err
		}
		if r.services.State != nil {
			raw, err := r.services.State.Get(ctx, name, target)
			if err != nil {
				return err
			}
			return r.printInvocationStatus(name, invocationStateView(raw), *jsonOutput)
		}
	} else if r.services.State != nil {
		lister, ok := r.services.State.(invocationLister)
		if !ok {
			return errors.New("durable invocation state does not support listing; use --invocation ID")
		}
		raw, err := lister.List(ctx, name, 100)
		if err != nil {
			return err
		}
		invocations := make([]InvocationState, 0, len(raw))
		for _, item := range raw {
			invocations = append(invocations, invocationStateView(item))
		}
		inv, ok := latestInvocation(invocations)
		if !ok {
			if *jsonOutput {
				return writeJSON(r.services.Stdout, InvocationState{SchemaVersion: 1, Function: name, Status: "unknown"})
			}
			fmt.Fprintln(r.services.Stdout, "Status: no invocations")
			return nil
		}
		return r.printInvocationStatus(name, inv, *jsonOutput)
	}
	if r.services.State == nil && r.services.ListInvocations != nil {
		invocations, err := r.services.ListInvocations(ctx, name, 100)
		if err != nil {
			return err
		}
		inv, ok := latestInvocation(invocations)
		if !ok {
			if *jsonOutput {
				return writeJSON(r.services.Stdout, InvocationState{SchemaVersion: 1, Function: name, Status: "unknown"})
			}
			fmt.Fprintln(r.services.Stdout, "Status: no invocations")
			return nil
		}
		return r.printInvocationStatus(name, inv, *jsonOutput)
	}

	client, err := r.client()
	if err != nil {
		return err
	}
	var run github.WorkflowRun
	var ok, ambiguous bool
	if target != "" {
		var runs []github.WorkflowRun
		if lister, supportsExact := client.(displayTitleLister); supportsExact {
			runs, err = lister.ListWorkflowRunsByDisplayTitle(ctx, r.services.WorkflowFor(name), canonicalDisplayTitle(target))
		} else {
			runs, err = client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 100)
		}
		if err != nil {
			return err
		}
		run, ok, ambiguous = matchExactRun(runs, target)
		if ambiguous {
			return fmt.Errorf("invocation %q is ambiguous", target)
		}
		if !ok {
			return fmt.Errorf("invocation %q not found", target)
		}
	} else {
		runs, listErr := client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 100)
		if listErr != nil {
			return listErr
		}
		run, ok = latestRun(runs)
		if !ok {
			if *jsonOutput {
				return writeJSON(r.services.Stdout, InvocationState{SchemaVersion: 1, Function: name, Status: "unknown"})
			}
			fmt.Fprintln(r.services.Stdout, "Status: no runs")
			return nil
		}
	}
	if *jsonOutput {
		return writeJSON(r.services.Stdout, invocationFromRun(name, target, run))
	}
	printRunStatus(r.services.Stdout, name, run)
	return nil
}

func latestInvocation(invocations []InvocationState) (InvocationState, bool) {
	if len(invocations) == 0 {
		return InvocationState{}, false
	}
	latest := invocations[0]
	for _, inv := range invocations[1:] {
		if inv.CreatedAt.After(latest.CreatedAt) ||
			(inv.CreatedAt.Equal(latest.CreatedAt) && inv.ID > latest.ID) {
			latest = inv
		}
	}
	return latest, true
}
func invocationStateView(inv state.Invocation) InvocationState {
	view := InvocationState{
		SchemaVersion: inv.SchemaVersion,
		Function:      inv.Function,
		ID:            inv.ID,
		Status:        string(inv.Status),
		Attempts:      inv.Attempts,
		MaxAttempts:   inv.MaxAttempts,
		CreatedAt:     inv.CreatedAt,
		StartedAt:     inv.StartedAt,
		CompletedAt:   inv.CompletedAt,
		LastError:     inv.LastError,
	}
	if inv.Provider != nil {
		view.Provider = &ProviderRef{RunID: inv.Provider.RunID, RunAttempt: inv.Provider.RunAttempt, Reason: inv.Provider.Reason}
	}
	return view
}

func (r *Root) printInvocationStatus(function string, inv InvocationState, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(r.services.Stdout, inv)
	}
	printStateStatus(r.services.Stdout, function, inv)
	return nil
}

func printStateStatus(out io.Writer, function string, inv InvocationState) {
	display, symbol := durableDisplayStatus(inv.Status)
	if strings.TrimSpace(inv.Function) != "" {
		function = inv.Function
	}
	fmt.Fprintln(out, function)
	fmt.Fprintf(out, "%s %s", symbol, display)
	if inv.Attempts > 0 {
		if inv.MaxAttempts > 0 {
			fmt.Fprintf(out, " (attempt %d/%d)", inv.Attempts, inv.MaxAttempts)
		} else {
			fmt.Fprintf(out, " (attempt %d)", inv.Attempts)
		}
	}
	if inv.StartedAt != nil && inv.CompletedAt != nil && inv.CompletedAt.After(*inv.StartedAt) {
		fmt.Fprintf(out, " in %s", humanDuration(inv.CompletedAt.Sub(*inv.StartedAt)))
	}
	fmt.Fprintln(out)
	if !inv.CreatedAt.IsZero() {
		fmt.Fprintf(out, "  %s\n", humanTimestamp(inv.CreatedAt))
	}
	if inv.Provider != nil && inv.Provider.RunID > 0 {
		fmt.Fprintf(out, "  run %d (attempt %d)\n", inv.Provider.RunID, inv.Provider.RunAttempt)
	}
	if strings.TrimSpace(inv.LastError) != "" {
		fmt.Fprintf(out, "  %s\n", strings.TrimSpace(inv.LastError))
	}
}

func durableDisplayStatus(raw string) (string, string) {
	if strings.EqualFold(strings.TrimSpace(raw), string(state.StatusPending)) {
		return "pending", "◆"
	}
	return displayStatus(raw)
}

func invocationFromRun(function, logicalID string, run github.WorkflowRun) InvocationState {
	rawStatus := strings.TrimSpace(run.Conclusion)
	if rawStatus == "" {
		rawStatus = strings.TrimSpace(run.Status)
	}
	status, _ := displayStatus(rawStatus)
	id := logicalID
	if id == "" {
		id = function + "/" + strconv.FormatInt(run.ID, 10)
	}
	return InvocationState{
		SchemaVersion: 1,
		Function:      function,
		ID:            id,
		Status:        status,
		CreatedAt:     run.CreatedAt,
		StartedAt:     run.StartedAt,
		CompletedAt:   &run.UpdatedAt,
		Provider:      &ProviderRef{RunID: run.ID, RunAttempt: run.RunAttempt},
	}
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func printRunStatus(out io.Writer, function string, run github.WorkflowRun) {
	rawStatus := strings.TrimSpace(run.Conclusion)
	if rawStatus == "" {
		rawStatus = strings.TrimSpace(run.Status)
	}
	status, symbol := displayStatus(rawStatus)
	fmt.Fprintln(out, function)
	fmt.Fprintf(out, "%s %s", symbol, status)
	if run.ID > 0 {
		fmt.Fprintf(out, " (run %d", run.ID)
		if run.RunAttempt > 0 {
			fmt.Fprintf(out, ", attempt %d", run.RunAttempt)
		}
		fmt.Fprint(out, ")")
	}
	start := run.CreatedAt
	if run.StartedAt != nil && !run.StartedAt.IsZero() {
		start = *run.StartedAt
	}
	end := run.UpdatedAt
	if !start.IsZero() && !end.IsZero() && end.After(start) {
		fmt.Fprintf(out, " in %s", humanDuration(end.Sub(start)))
	}
	fmt.Fprintln(out)
	if !start.IsZero() {
		fmt.Fprintf(out, "  %s\n", humanTimestamp(start))
	}
	if sanitized := sanitizeURL(run.HTMLURL); sanitized != "" {
		fmt.Fprintf(out, "  %s\n", sanitized)
	}
}

func displayStatus(raw string) (string, string) {
	switch strings.ToLower(raw) {
	case "success", "succeeded":
		return "succeeded", "✓"
	case "failure", "failed", "cancelled", "timed_out", "action_required", "stale":
		return "failed", "×"
	case "queued", "requested", "waiting", "pending", "in_progress", "running":
		return "running", "•"
	case "skipped", "neutral":
		return "skipped", "·"
	default:
		if raw == "" {
			return "unknown", "•"
		}
		return raw, "•"
	}
}

func humanTimestamp(value time.Time) string {
	return value.Format("2006-01-02 15:04 MST")
}

func sanitizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	h := d / time.Hour
	d %= time.Hour
	m := d / time.Minute
	d %= time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

func (r *Root) logs(ctx context.Context, args []string) error {
	fs := newFlags("logs")
	invocationID := fs.String("invocation", "", "exact logical invocation ID or provider run ID")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid logs options")
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas logs FUNCTION [--invocation ID]")
	}
	name := fs.Arg(0)
	m, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := selectedFunctions(m, name); err != nil {
		return err
	}
	target := strings.TrimSpace(*invocationID)
	var runID int64
	var runAttempt int
	if target != "" {
		if parsed, parseErr := strconv.ParseInt(target, 10, 64); parseErr == nil && parsed > 0 {
			runID = parsed
		} else {
			if err := validateLogicalID(name, target); err != nil {
				return err
			}
			if r.services.State != nil {
				raw, err := r.services.State.Get(ctx, name, target)
				if err != nil {
					return err
				}
				inv := invocationStateView(raw)
				if inv.Provider == nil || inv.Provider.RunID <= 0 {
					return fmt.Errorf("invocation %q has no provider workflow run", target)
				}
				runID = inv.Provider.RunID
				runAttempt = inv.Provider.RunAttempt
			}
		}
	} else if r.services.State != nil {
		lister, ok := r.services.State.(invocationLister)
		if !ok {
			return errors.New("durable invocation state does not support listing; use --invocation ID")
		}
		raw, err := lister.List(ctx, name, 100)
		if err != nil {
			return err
		}
		invocations := make([]InvocationState, 0, len(raw))
		for _, item := range raw {
			invocations = append(invocations, invocationStateView(item))
		}
		inv, ok := latestInvocation(invocations)
		if !ok || inv.Provider == nil || inv.Provider.RunID <= 0 {
			return fmt.Errorf("no workflow run for %s", name)
		}
		runID = inv.Provider.RunID
		runAttempt = inv.Provider.RunAttempt
	}
	if target == "" && runID == 0 && r.services.State == nil && r.services.ListInvocations != nil {
		invocations, err := r.services.ListInvocations(ctx, name, 100)
		if err != nil {
			return err
		}
		inv, ok := latestInvocation(invocations)
		if !ok || inv.Provider == nil || inv.Provider.RunID <= 0 {
			return fmt.Errorf("no workflow run for %s", name)
		}
		runID = inv.Provider.RunID
		runAttempt = inv.Provider.RunAttempt
	}

	client, err := r.client()
	if err != nil {
		return err
	}
	if runID == 0 {
		var runs []github.WorkflowRun
		if target != "" {
			if lister, supportsExact := client.(displayTitleLister); supportsExact {
				runs, err = lister.ListWorkflowRunsByDisplayTitle(ctx, r.services.WorkflowFor(name), canonicalDisplayTitle(target))
			} else {
				runs, err = client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 100)
			}
		} else {
			runs, err = client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 100)
		}
		if err != nil {
			return err
		}
		var run github.WorkflowRun
		var ok, ambiguous bool
		if target != "" {
			run, ok, ambiguous = matchExactRun(runs, target)
			if ambiguous {
				return fmt.Errorf("invocation %q is ambiguous", target)
			}
			if !ok {
				return fmt.Errorf("invocation %q not found", target)
			}
		} else {
			run, ok = latestRun(runs)
			if !ok {
				return fmt.Errorf("no workflow runs for %s", name)
			}
		}
		runID = run.ID
		runAttempt = run.RunAttempt
	}
	if runID <= 0 {
		return fmt.Errorf("no workflow run for %s", name)
	}
	var reader io.ReadCloser
	if runAttempt > 0 {
		if getter, ok := client.(attemptLogsGetter); ok {
			reader, err = getter.GetWorkflowAttemptLogs(ctx, runID, runAttempt)
			if err != nil {
				return err
			}
		}
	}
	if reader == nil {
		reader, err = client.GetWorkflowLogs(ctx, runID)
	}
	if err != nil {
		return err
	}
	defer reader.Close()
	return github.ReadLogs(r.services.Stdout, reader)
}
