package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urnetwork/build/all/acceptance/testconfig"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "test-config: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	flags := flag.NewFlagSet("test-config", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to vault/main/tests.yml")
	ready := flags.Bool("ready", false, "require every acceptance fixture to be configured")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *configPath == "" || flags.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: test-config --config FILE [--ready] validate|get PATH|write-json FILE")
		os.Exit(2)
	}

	config, err := testconfig.Load(*configPath)
	if err != nil {
		fail("%v", err)
	}
	command := flags.Arg(0)
	switch command {
	case "validate":
		if flags.NArg() != 1 {
			fail("validate takes no arguments")
		}
		if err := config.Validate(*ready); err != nil {
			fail("%v", err)
		}
		if *ready {
			if err := config.ValidateProvisioned(*configPath); err != nil {
				fail("%v", err)
			}
		}
	case "get":
		if flags.NArg() != 2 {
			fail("get requires one config path")
		}
		value, err := config.Get(flags.Arg(1))
		if err != nil {
			fail("%v", err)
		}
		if flags.Arg(1) == "providers.google.browser_profile" ||
			flags.Arg(1) == "providers.apple.browser_profile" {
			value = testconfig.ResolveProfile(*configPath, value)
			if value != "" {
				value, err = filepath.Abs(value)
				if err != nil {
					fail("resolve profile: %v", err)
				}
			}
		}
		fmt.Print(value)
	case "write-json":
		if flags.NArg() != 2 {
			fail("write-json requires one output path")
		}
		if err := config.Validate(true); err != nil {
			fail("%v", err)
		}
		if err := config.ValidateProvisioned(*configPath); err != nil {
			fail("%v", err)
		}
		output, err := filepath.Abs(flags.Arg(1))
		if err != nil {
			fail("resolve output: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			fail("create output directory: %v", err)
		}
		temporary := output + ".tmp"
		file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fail("open output: %v", err)
		}
		resolved := config.ResolveProfiles(*configPath)
		encoder := json.NewEncoder(file)
		if err := encoder.Encode(&resolved); err != nil {
			_ = file.Close()
			_ = os.Remove(temporary)
			fail("encode output: %v", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(temporary)
			fail("close output: %v", err)
		}
		if err := os.Rename(temporary, output); err != nil {
			_ = os.Remove(temporary)
			fail("install output: %v", err)
		}
		if err := os.Chmod(output, 0o600); err != nil {
			fail("secure output: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		os.Exit(2)
	}
}
