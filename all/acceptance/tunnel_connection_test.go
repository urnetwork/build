// Exercises desktop startup ordering with synthetic service replies. No test
// opens a socket, reads a host setting, or waits for a wall-clock interval.
package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Replaces only the control operations the startup orchestration may call.
type tunnelConnectionControl struct {
	platformControl
	start  func(context.Context, tunnelConfig) (tunnelStatus, error)
	status func(context.Context) (tunnelStatus, error)
}

// Returns the synthetic start reply without touching a daemon.
func (self *tunnelConnectionControl) Start(ctx context.Context, config tunnelConfig) (tunnelStatus, error) {
	return self.start(ctx, config)
}

// Advances a test-owned service state instead of observing host routes.
func (self *tunnelConnectionControl) Status(ctx context.Context) (tunnelStatus, error) {
	return self.status(ctx)
}

// Uses the real Windows decoder so capture checks also cover wire fields.
func windowsTunnelTestStatus(state string, routesInstalled bool, dnsApplied bool) tunnelStatus {
	return windowsStatus(map[string]any{"status": map[string]any{
		"state":            state,
		"mode":             "tunnel",
		"protocol_version": float64(4),
		"routes_installed": routesInstalled,
		"dns_applied":      dnsApplied,
	}})
}

// Reproduces the v4 dependency: the service cannot reach Up before this
// consumer connects device RPC and selects a provider. The old ordering
// deterministically hits the pre-selection status error below.
func TestWindowsTunnelSelectsProviderBeforeWaitingForCapture(t *testing.T) {
	var events []string
	providerSelected := false
	captureObserved := false
	control := &tunnelConnectionControl{
		start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
			events = append(events, "start")
			return windowsTunnelTestStatus("preparing", false, false), nil
		},
		status: func(context.Context) (tunnelStatus, error) {
			if !providerSelected {
				return tunnelStatus{}, errors.New("capture was awaited before device RPC selected a provider")
			}
			events = append(events, "capture")
			captureObserved = true
			return windowsTunnelTestStatus("up", true, true), nil
		},
	}
	ip, err := startTunnelAndCheckEgress(t.Context(), "windows", control, tunnelConfig{}, func() error {
		events = append(events, "device RPC", "select provider")
		providerSelected = true
		return nil
	}, func() (string, error) {
		if !captureObserved {
			t.Fatal("public egress was measured before capture")
		}
		events = append(events, "egress")
		return "203.0.113.9", nil
	})
	if err != nil || ip != "203.0.113.9" {
		t.Fatalf("egress = %q, %v", ip, err)
	}
	want := []string{"start", "device RPC", "select provider", "capture", "egress"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("startup events = %v, want %v", events, want)
	}
}

// A live RPC with no proven provider must remain pending after selection.
// Cancellation supplies the explicit boundary; no polling sleep can decide it.
func TestWindowsPreparingDoesNotAuthorizeEgress(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	providerSelected := false
	control := &tunnelConnectionControl{
		start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
			return windowsTunnelTestStatus("preparing", false, false), nil
		},
		status: func(context.Context) (tunnelStatus, error) {
			if !providerSelected {
				t.Fatal("status read before provider selection")
			}
			cancel()
			return windowsTunnelTestStatus("preparing", false, false), nil
		},
	}
	_, err := startTunnelAndCheckEgress(ctx, "windows", control, tunnelConfig{}, func() error {
		providerSelected = true
		return nil
	}, func() (string, error) {
		t.Fatal("preparing tunnel authorized an egress check")
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pending capture = %v, want cancellation", err)
	}
}

// Known live states still require the exact Windows protocol and tunnel mode;
// failed, unknown, or prematurely capturing starts never create device RPC.
func TestWindowsTunnelRejectsInvalidBootstrapReplies(t *testing.T) {
	statuses := []tunnelStatus{
		windowsTunnelTestStatus("stopped", false, false),
		windowsTunnelTestStatus("stopping", false, false),
		windowsTunnelTestStatus("error", false, false),
		windowsTunnelTestStatus("rpc_only", false, false),
		windowsTunnelTestStatus("unexpected", false, false),
		windowsTunnelTestStatus("preparing", true, false),
		windowsTunnelTestStatus("preparing", false, true),
	}
	for _, version := range []int{0, 1, 3, 5} {
		status := windowsTunnelTestStatus("preparing", false, false)
		status.ProtocolVersion = version
		statuses = append(statuses, status)
	}
	status := windowsTunnelTestStatus("preparing", false, false)
	status.Mode = "rpc_only"
	statuses = append(statuses, status)
	status = windowsTunnelTestStatus("preparing", false, false)
	status.Error = "synthetic bootstrap failure"
	statuses = append(statuses, status)
	for _, status := range statuses {
		control := &tunnelConnectionControl{
			start: func(context.Context, tunnelConfig) (tunnelStatus, error) { return status, nil },
			status: func(context.Context) (tunnelStatus, error) {
				t.Fatalf("invalid bootstrap was polled: %+v", status)
				return tunnelStatus{}, nil
			},
		}
		_, err := startTunnelAndCheckEgress(t.Context(), "windows", control, tunnelConfig{}, func() error {
			t.Fatalf("invalid bootstrap created device RPC: %+v", status)
			return nil
		}, func() (string, error) {
			t.Fatalf("invalid bootstrap authorized egress: %+v", status)
			return "", nil
		})
		if err == nil {
			t.Fatalf("invalid bootstrap accepted: %+v", status)
		}
	}
}

// Up alone is insufficient: the post-selection reply must confirm both routes
// and DNS. Missing wire facts, errors, or protocol changes cannot pass.
func TestWindowsTunnelRejectsFailedOrIncompleteCapture(t *testing.T) {
	statuses := []tunnelStatus{
		windowsTunnelTestStatus("up", false, false),
		windowsTunnelTestStatus("up", true, false),
		windowsTunnelTestStatus("up", false, true),
		windowsTunnelTestStatus("error", false, false),
		windowsTunnelTestStatus("stopped", false, false),
		windowsTunnelTestStatus("unexpected", true, true),
		windowsStatus(map[string]any{"status": map[string]any{
			"state": "up", "mode": "tunnel", "protocol_version": float64(4),
		}}),
	}
	status := windowsTunnelTestStatus("up", true, true)
	status.ProtocolVersion = 3
	statuses = append(statuses, status)
	status = windowsTunnelTestStatus("up", true, true)
	status.Error = "synthetic activation failure"
	statuses = append(statuses, status)
	for _, status := range statuses {
		providerSelected := false
		control := &tunnelConnectionControl{
			start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
				return windowsTunnelTestStatus("preparing", false, false), nil
			},
			status: func(context.Context) (tunnelStatus, error) {
				if !providerSelected {
					t.Fatal("capture read preceded provider selection")
				}
				return status, nil
			},
		}
		_, err := startTunnelAndCheckEgress(t.Context(), "windows", control, tunnelConfig{}, func() error {
			providerSelected = true
			return nil
		}, func() (string, error) {
			t.Fatalf("failed capture authorized egress: %+v", status)
			return "", nil
		})
		if err == nil || !providerSelected {
			t.Fatalf("capture = %+v, selected=%t error=%v", status, providerSelected, err)
		}
	}
}

// Linux v1 still waits for Up before opening device RPC and does not require
// Windows-only status fields on either its start or later status replies.
func TestLinuxTunnelRetainsUpBeforeProviderSelection(t *testing.T) {
	var events []string
	control := &tunnelConnectionControl{
		start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
			events = append(events, "start")
			return tunnelStatus{State: "starting"}, nil
		},
		status: func(context.Context) (tunnelStatus, error) {
			events = append(events, "up")
			return tunnelStatus{State: "up"}, nil
		},
	}
	_, err := startTunnelAndCheckEgress(t.Context(), "linux", control, tunnelConfig{}, func() error {
		events = append(events, "select provider")
		return nil
	}, func() (string, error) {
		events = append(events, "egress")
		return "198.51.100.9", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "up", "select provider", "up", "egress"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("Linux startup = %v, want %v", events, want)
	}
}

// Linux does not silently adopt the new Windows state through a shared helper.
func TestLinuxTunnelRejectsPreparing(t *testing.T) {
	control := &tunnelConnectionControl{
		start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
			return tunnelStatus{State: "preparing"}, nil
		},
	}
	_, err := startTunnelAndCheckEgress(t.Context(), "linux", control, tunnelConfig{}, func() error {
		t.Fatal("Linux preparing state opened device RPC")
		return nil
	}, func() (string, error) {
		t.Fatal("Linux preparing state authorized egress")
		return "", nil
	})
	if err == nil {
		t.Fatal("Linux accepted preparing")
	}
}

// A failed start, provider selection, or later status call must terminate the
// sequence at that stage without measuring native egress as tunnel success.
func TestTunnelStartupStopsAtFailedStage(t *testing.T) {
	for _, stage := range []string{"start", "provider", "status"} {
		failure := errors.New("synthetic " + stage + " failure")
		control := &tunnelConnectionControl{
			start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
				if stage == "start" {
					return tunnelStatus{}, failure
				}
				return windowsTunnelTestStatus("preparing", false, false), nil
			},
			status: func(context.Context) (tunnelStatus, error) {
				if stage != "status" {
					t.Fatalf("%s failure continued to status", stage)
				}
				return tunnelStatus{}, failure
			},
		}
		_, err := startTunnelAndCheckEgress(t.Context(), "windows", control, tunnelConfig{}, func() error {
			if stage == "start" {
				t.Fatal("failed start continued to device RPC")
			}
			if stage == "provider" {
				return failure
			}
			return nil
		}, func() (string, error) {
			t.Fatalf("%s failure authorized egress", stage)
			return "", nil
		})
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), stage) {
			t.Fatalf("%s failure returned %v", stage, err)
		}
	}
}

// Cancellation wins over an otherwise ready start/status reply and prevents
// the next SDK or public-network operation from starting.
func TestTunnelStartupCancellationStopsTheNextOperation(t *testing.T) {
	for _, test := range []struct {
		stage  string
		events []string
	}{
		{stage: "before start", events: []string{}},
		{stage: "start", events: []string{"start"}},
		{stage: "provider", events: []string{"start", "provider"}},
		{stage: "status", events: []string{"start", "provider", "status"}},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		events := []string{}
		control := &tunnelConnectionControl{
			start: func(context.Context, tunnelConfig) (tunnelStatus, error) {
				events = append(events, "start")
				if test.stage == "start" {
					cancel()
				}
				return windowsTunnelTestStatus("preparing", false, false), nil
			},
			status: func(context.Context) (tunnelStatus, error) {
				events = append(events, "status")
				if test.stage == "status" {
					cancel()
				}
				return windowsTunnelTestStatus("up", true, true), nil
			},
		}
		if test.stage == "before start" {
			cancel()
		}
		_, err := startTunnelAndCheckEgress(ctx, "windows", control, tunnelConfig{}, func() error {
			events = append(events, "provider")
			if test.stage == "provider" {
				cancel()
			}
			return nil
		}, func() (string, error) {
			t.Fatalf("cancellation at %s authorized egress", test.stage)
			return "", nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation at %s = %v", test.stage, err)
		}
		if !reflect.DeepEqual(events, test.events) {
			t.Fatalf("cancellation at %s events = %v, want %v", test.stage, events, test.events)
		}
	}
}
