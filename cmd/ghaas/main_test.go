package main

import (
	"context"
	"strings"
	"testing"
)

func TestExecuteRejectsNilContext(t *testing.T) {
	if code, err := execute(nil, []string{"--help"}); code != 1 || err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("execute(nil) = %d, %v", code, err)
	}
}

func TestExecuteHelpAndVersion(t *testing.T) {
	if code, err := execute(context.Background(), []string{"--help"}); code != 0 || err != nil {
		t.Fatalf("help = %d, %v", code, err)
	}
	if code, err := execute(context.Background(), []string{"version"}); code != 0 || err != nil {
		t.Fatalf("version = %d, %v", code, err)
	}
}

func TestExecuteRuntimeHelpInAnyArgumentPosition(t *testing.T) {
	for _, args := range [][]string{
		{"runtime", "invoke", "-h", "hello"},
		{"runtime", "invoke", "hello", "--help"},
		{"runtime", "invoke", "--help", "hello", "--unknown"},
	} {
		if code, err := execute(context.Background(), args); code != 0 || err != nil {
			t.Fatalf("runtime help %v = %d, %v", args, code, err)
		}
	}
}
