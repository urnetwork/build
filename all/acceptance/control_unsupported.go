//go:build !linux && !windows

// Unsupported hosts can compile unit tests but cannot drive a desktop service.
package main

import (
	"context"
	"fmt"
	"runtime"
)

// Rejects runtime use outside the two supported service platforms.
func newPlatformControl(context.Context) (platformControl, error) {
	return nil, fmt.Errorf("acceptance control is not supported on %s", runtime.GOOS)
}
