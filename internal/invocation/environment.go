package invocation

import (
	"sort"
	"strconv"
)

// InvocationEnvironment returns only the non-secret metadata exposed to a
// function process. A new map is returned on every call.
func InvocationEnvironment(i Invocation) map[string]string {
	trigger := i.Trigger
	if trigger == "" {
		trigger = "unknown"
	}
	runID := ""
	if i.WorkflowRunID > 0 {
		runID = strconv.FormatInt(i.WorkflowRunID, 10)
	}
	return map[string]string{
		"GHAAS_FUNCTION":        i.Function,
		"GHAAS_INVOCATION_ID":   string(i.ID),
		"GHAAS_ATTEMPT":         strconv.Itoa(i.Attempts),
		"GHAAS_TRIGGER":         trigger,
		"GHAAS_WORKFLOW_RUN_ID": runID,
	}
}

// BuildEnvironment constructs GHAAS_* metadata without requiring a complete
// state record. It is intended for workflow/runtime adapters.
func BuildEnvironment(function string, id InvocationID, attempt int, trigger string, workflowRunID int64) map[string]string {
	return InvocationEnvironment(Invocation{
		Function: function, ID: id, Attempts: attempt, Trigger: trigger,
		WorkflowRunID: workflowRunID,
	})
}

// EnvironmentForInvocation is a descriptive alias for InvocationEnvironment.
func EnvironmentForInvocation(i Invocation) map[string]string {
	return InvocationEnvironment(i)
}

// MergeEnvironment applies values over base while retaining argument-safe
// KEY=VALUE boundaries. Values supplied by the invocation always win.
func MergeEnvironment(base []string, values map[string]string) []string {
	if len(values) == 0 {
		return append([]string(nil), base...)
	}
	result := make([]string, 0, len(base)+len(values))
	seen := make(map[string]struct{}, len(values))
	for _, item := range base {
		key := item
		if idx := indexByte(item, '='); idx >= 0 {
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
func indexByte(s string, c byte) int {
	for n := range len(s) {
		if s[n] == c {
			return n
		}
	}
	return -1
}
