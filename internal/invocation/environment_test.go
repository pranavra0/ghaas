package invocation

import "testing"

func TestMetadataEnvironment(t *testing.T) {
	got := (Metadata{
		Function: "hello", ID: "hello/2026-08-29", Attempt: 1,
		Trigger: "schedule", WorkflowRunID: 42,
	}).Environment()
	want := map[string]string{
		"GHAAS_FUNCTION":        "hello",
		"GHAAS_INVOCATION_ID":   "hello/2026-08-29",
		"GHAAS_ATTEMPT":         "1",
		"GHAAS_TRIGGER":         "schedule",
		"GHAAS_WORKFLOW_RUN_ID": "42",
	}
	if len(got) != len(want) {
		t.Fatalf("metadata environment has %d values, want %d", len(got), len(want))
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("metadata %s = %q, want %q", key, got[key], value)
		}
	}
}
