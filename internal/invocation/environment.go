package invocation

import (
	"sort"
	"strconv"
	"strings"
)

// Metadata is the non-secret context exposed to a function process. These
// values are supplied by the generated workflow or runtime entrypoint; they
// are not persisted by this package.
type Metadata struct {
	Function      string
	ID            string
	Attempt       int
	Trigger       string
	WorkflowRunID int64
}

// Environment returns the GHAAS_* variables emitted for a command invocation.
// Metadata values are always represented as strings because they cross an
// operating-system process boundary.
func (m Metadata) Environment() map[string]string {
	trigger := m.Trigger
	if trigger == "" {
		trigger = "unknown"
	}
	runID := ""
	if m.WorkflowRunID > 0 {
		runID = strconv.FormatInt(m.WorkflowRunID, 10)
	}
	return map[string]string{
		"GHAAS_FUNCTION":        m.Function,
		"GHAAS_INVOCATION_ID":   m.ID,
		"GHAAS_ATTEMPT":         strconv.Itoa(m.Attempt),
		"GHAAS_TRIGGER":         trigger,
		"GHAAS_WORKFLOW_RUN_ID": runID,
	}
}

// MergeEnvironment applies values over base while retaining argument-safe
// KEY=VALUE boundaries. Values supplied by the invocation always win. The
// returned slice never aliases base.
func MergeEnvironment(base []string, values map[string]string) []string {
	if len(values) == 0 {
		return append([]string(nil), base...)
	}
	result := make([]string, 0, len(base)+len(values))
	seen := make(map[string]struct{}, len(values))
	for _, item := range base {
		key := item
		if idx := strings.IndexByte(item, '='); idx >= 0 {
			key = item[:idx]
		}
		if value, ok := values[key]; ok {
			result = append(result, key+"="+value)
			seen[key] = struct{}{}
		} else {
			result = append(result, item)
		}
	}
	keys := make([]string, 0, len(values)-len(seen))
	for key := range values {
		if _, ok := seen[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
