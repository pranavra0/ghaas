package invocation

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NewScheduledID builds the stable ID for one scheduled key.
func NewScheduledID(function, scheduleKey string) (InvocationID, error) {
	if err := validateComponent("function", function); err != nil {
		return "", err
	}
	if err := validateComponent("schedule key", scheduleKey); err != nil {
		return "", err
	}
	return InvocationID(function + "/" + scheduleKey), nil
}

// ScheduledID is a concise spelling of NewScheduledID.
func ScheduledID(function, scheduleKey string) (InvocationID, error) {
	return NewScheduledID(function, scheduleKey)
}

// ValidateUUID accepts the canonical 8-4-4-4-12 hexadecimal UUID form.
func ValidateUUID(uuid string) error {
	if !uuidPattern.MatchString(uuid) {
		return fmt.Errorf("invalid UUID %q", uuid)
	}
	return nil
}

// NewManualID builds an invocation ID from a canonical UUID. UUIDs are
// normalized to lowercase to make equivalent inputs produce one ID.
func NewManualID(function, uuid string) (InvocationID, error) {
	if err := validateComponent("function", function); err != nil {
		return "", err
	}
	if err := ValidateUUID(uuid); err != nil {
		return "", err
	}
	return InvocationID(function + "/" + strings.ToLower(uuid)), nil
}
// NewScheduledInvocationID is an explicit spelling of NewScheduledID.
func NewScheduledInvocationID(function, scheduleKey string) (InvocationID, error) {
	return NewScheduledID(function, scheduleKey)
}

// NewManualInvocationID is an explicit spelling of NewManualID.
func NewManualInvocationID(function, uuid string) (InvocationID, error) {
	return NewManualID(function, uuid)
}

// ManualID is a concise spelling of NewManualID.
func ManualID(function, uuid string) (InvocationID, error) {
	return NewManualID(function, uuid)
}

// ValidateInvocationID validates the path-safe function/key representation.
// It intentionally does not require the suffix to be a UUID: scheduled IDs
// use calendar keys.
func ValidateInvocationID(id InvocationID) error {
	parts := strings.Split(string(id), "/")
	if len(parts) != 2 {
		return fmt.Errorf("invocation ID must be function/key")
	}
	if err := validateComponent("function", parts[0]); err != nil {
		return err
	}
	if err := validateComponent("invocation key", parts[1]); err != nil {
		return err
	}
	return nil
}

func validateComponent(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || value == "." || value == ".." {
		return fmt.Errorf("%s contains a path separator", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}
