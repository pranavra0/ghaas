package invocation

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	functionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// NewScheduledID builds the stable ID for one scheduled key.
func NewScheduledID(function, scheduleKey string) (InvocationID, error) {
	if err := validateFunctionName(function); err != nil {
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
	if err := validateFunctionName(function); err != nil {
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

// ValidateInvocationID validates the canonical function/key representation.
// The function component follows manifest function naming rules. The key is
// deliberately less restrictive because scheduled keys are user-defined, but
// it must remain a single path-safe, whitespace-free component. UUID keys are
// canonicalized to lowercase by NewManualID and are rejected otherwise.
func ValidateInvocationID(id InvocationID) error {
	parts := strings.Split(string(id), "/")
	if len(parts) != 2 {
		return fmt.Errorf("invocation ID must be function/key")
	}
	if err := validateFunctionComponent(parts[0]); err != nil {
		return err
	}
	if err := validateComponent("invocation key", parts[1]); err != nil {
		return err
	}
	if uuidPattern.MatchString(parts[1]) && parts[1] != strings.ToLower(parts[1]) {
		return fmt.Errorf("invocation key UUID must be lowercase")
	}
	return nil
}

func validateFunctionComponent(value string) error {
	if value == "" {
		return fmt.Errorf("function is required")
	}
	if !functionPattern.MatchString(value) {
		return fmt.Errorf("function %q is not canonical", value)
	}
	return nil
}

func validateFunctionName(value string) error {
	return validateFunctionComponent(value)
}

func validateInvocationIdentity(function string, id InvocationID) error {
	if err := validateFunctionName(function); err != nil {
		return err
	}
	if err := ValidateInvocationID(id); err != nil {
		return err
	}
	parts := strings.Split(string(id), "/")
	if parts[0] != function {
		return fmt.Errorf("%w: invocation ID %q does not belong to function %q", ErrInvalidInvocation, id, function)
	}
	return nil
}

func validateComponent(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s contains surrounding whitespace", name)
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") || value == "." || value == ".." {
		return fmt.Errorf("%s contains a path separator", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("%s contains whitespace or a control character", name)
		}
	}
	return nil
}
