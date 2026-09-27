package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/urnetwork/build/all/acceptance/authcases"
)

// Run has already executed every requested auth case. Preserve and report all
// of those outcomes before deciding whether to enter the data-plane phase.
func reportAuthCases(stderr io.Writer, results []authcases.Result) error {
	var failures []error
	for _, result := range results {
		fmt.Fprintf(stderr, "acceptance: auth case %s: %s\n", result.Case, result.Status)
		if result.Status != "PASS" {
			failures = append(failures, fmt.Errorf("auth case %s: %s", result.Case, result.Detail))
		}
	}
	return errors.Join(failures...)
}

// A nonzero process exit must not discard checks which already ran. Complete
// covers the agent's requested cases, not the enclosing platform's teardown.
// Repetitions remains the requested count; CompletedRepetitions is evidence
// only for fully successful tunnel iterations. No raw error is added to JSON:
// auth details have already passed the auth runner's fixture redaction.
func writeAcceptanceResult(stdout, stderr io.Writer, result *acceptanceResult, runErr error) int {
	if result == nil && runErr == nil {
		runErr = errors.New("acceptance produced no result")
	}
	if runErr != nil {
		if result != nil {
			result.Ok = false
			result.Complete = false
		}
		fmt.Fprintf(stderr, "acceptance: %v\n", runErr)
	}
	if result != nil {
		if !result.Complete {
			result.Ok = false
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "acceptance: encode result: %v\n", err)
			return 1
		}
	}
	if runErr != nil || result == nil || !result.Ok || !result.Complete {
		return 1
	}
	return 0
}
