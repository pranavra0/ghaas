package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pranavra0/ghaas/internal/github"
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
	client, err := r.client()
	if err != nil {
		return err
	}
	uuid, err := newUUID()
	if err != nil {
		return err
	}
	logicalID := name + "/" + uuid
	if err := client.DispatchWorkflow(ctx, r.services.WorkflowFor(name), *ref, map[string]string{"ghaas_invocation_id": uuid}); err != nil {
		return err
	}
	fmt.Fprintf(r.services.Stdout, "◆ %s  dispatched\n  %s\n", name, logicalID)
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

func (r *Root) status(ctx context.Context, args []string) error {
	fs := newFlags("status")
	invocationID := fs.String("invocation", "", "exact logical invocation ID")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid status options")
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas status FUNCTION [--invocation ID]")
	}
	name := fs.Arg(0)
	m, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := selectedFunctions(m, name); err != nil {
		return err
	}
	client, err := r.client()
	if err != nil {
		return err
	}
	target := strings.TrimSpace(*invocationID)
	var run github.WorkflowRun
	var ok, ambiguous bool
	if target != "" {
		if err := validateLogicalID(name, target); err != nil {
			return err
		}
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
			fmt.Fprintln(r.services.Stdout, "Status: no runs")
			return nil
		}
	}
	printRunStatus(r.services.Stdout, name, run)
	return nil
}

func printRunStatus(out io.Writer, function string, run github.WorkflowRun) {
	rawStatus := strings.TrimSpace(run.Conclusion)
	if rawStatus == "" {
		rawStatus = strings.TrimSpace(run.Status)
	}
	status, symbol := displayStatus(rawStatus)
	fmt.Fprintln(out, function)
	fmt.Fprintf(out, "%s %s", symbol, status)

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
	client, err := r.client()
	if err != nil {
		return err
	}
	target := strings.TrimSpace(*invocationID)
	var runID int64
	if target != "" {
		if _, parseErr := strconv.ParseInt(target, 10, 64); parseErr != nil {
			if err := validateLogicalID(name, target); err != nil {
				return err
			}
		}
		if parsed, parseErr := strconv.ParseInt(target, 10, 64); parseErr == nil && parsed > 0 {
			runID = parsed
		}
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
	}
	if runID <= 0 {
		return fmt.Errorf("no workflow run for %s", name)
	}
	reader, err := client.GetWorkflowLogs(ctx, runID)
	if err != nil {
		return err
	}
	defer reader.Close()
	return github.ReadLogs(r.services.Stdout, reader)
}
