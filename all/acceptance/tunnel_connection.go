// Desktop startup separates the live device RPC from confirmed host capture.
// Windows cannot capture until this client selects a provider through that RPC;
// Linux v1 continues to establish its tunnel before exposing the device.
package main

import (
	"context"
	"fmt"
	"time"
)

// Drives the real start, provider selection, capture, and egress ordering.
// The SDK and public-address callbacks let tests enforce it without accounts,
// network access, or changes to the host's routes.
func startTunnelAndCheckEgress(
	ctx context.Context,
	platform string,
	control platformControl,
	config tunnelConfig,
	connectProvider func() error,
	checkEgress func() (string, error),
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	startCtx, startCancel := context.WithTimeout(ctx, 90*time.Second)
	started, err := control.Start(startCtx, config)
	startCancel()
	if err != nil {
		return "", fmt.Errorf("start tunnel: %w", err)
	}

	bootstrapCtx, bootstrapCancel := context.WithTimeout(ctx, 30*time.Second)
	err = waitForTunnelReadiness(bootstrapCtx, platform, control, &started, false)
	bootstrapCancel()
	if err != nil {
		return "", fmt.Errorf("tunnel device RPC did not become ready: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := connectProvider(); err != nil {
		return "", err
	}

	// A fresh status after selection prevents a prepared RPC or stale start
	// reply from being treated as evidence that public traffic is captured.
	captureCtx, captureCancel := context.WithTimeout(ctx, 30*time.Second)
	err = waitForTunnelReadiness(captureCtx, platform, control, nil, true)
	captureCancel()
	if err != nil {
		return "", fmt.Errorf("tunnel did not activate capture: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ip, err := checkEgress()
	if err != nil {
		return "", fmt.Errorf("network egress: %w", err)
	}
	return ip, nil
}

// Reads immediately, then polls with both an overall and per-call deadline.
// An initial start reply is usable only for the device-bootstrap phase.
func waitForTunnelReadiness(ctx context.Context, platform string, control platformControl, initial *tunnelStatus, capture bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if initial != nil {
		if ready, err := tunnelReady(platform, *initial, capture); ready || err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := control.Status(callCtx)
		cancel()
		if err != nil {
			return err
		}
		if ready, err := tunnelReady(platform, status, capture); ready || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Accepts only the platform's known live states. Preparing is a Windows RPC
// boundary, never proof of host routing; Linux's unchanged boundary is Up.
func tunnelReady(platform string, status tunnelStatus, capture bool) (bool, error) {
	if status.Error != "" {
		return false, fmt.Errorf("state=%s error=%s", status.State, status.Error)
	}
	switch platform {
	case "windows":
		if err := validateControlProtocol(platform, status.ProtocolVersion); err != nil {
			return false, err
		}
		if status.Mode != "tunnel" {
			return false, fmt.Errorf("unexpected Windows tunnel mode %q", status.Mode)
		}
	case "linux":
		// Linux v1 negotiates its protocol in Hello, not in every status.
	default:
		return false, fmt.Errorf("unsupported control platform %q", platform)
	}

	switch status.State {
	case "starting":
		return false, nil
	case "preparing":
		if platform != "windows" {
			return false, fmt.Errorf("unexpected %s tunnel state %q", platform, status.State)
		}
		if !capture && (status.RoutesInstalled || status.DnsApplied) {
			return false, fmt.Errorf("preparing tunnel already changed routes or DNS")
		}
		return !capture, nil
	case "up":
		if platform == "windows" && (!status.RoutesInstalled || !status.DnsApplied) {
			return false, fmt.Errorf("up tunnel is incomplete: routes_installed=%t dns_applied=%t", status.RoutesInstalled, status.DnsApplied)
		}
		return true, nil
	default:
		return false, fmt.Errorf("unexpected %s tunnel state %q", platform, status.State)
	}
}
