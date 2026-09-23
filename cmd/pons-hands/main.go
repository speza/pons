// Command pons-hands serves the built-in hands catalog over
// tool_provider/v1. It contains no brain, transcript, or model credentials and
// is intended to be launched by an execution-environment provider.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/samperrin/pons/internal/toolhost"
)

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }

func (s *stringsFlag) Set(value string) error {
	if value == "" {
		return errors.New("path must not be empty")
	}
	*s = append(*s, value)
	return nil
}

func main() {
	workspace := flag.String("workspace", "", "explicit workspace root (required)")
	fsReadBytes := flag.Int("fs-read-bytes", 0, "read_file byte cap; 0 = default")
	bashTimeout := flag.Int("bash-timeout", 60, "default bash timeout seconds")
	bashMaxLines := flag.Int("bash-max-lines", 200, "bash output line cap")
	bashMaxBytes := flag.Int("bash-max-bytes", 50*1024, "bash output byte cap")
	pluginPath := flag.String("plugin-path", "", "PATH supplied to nested external providers")
	pluginMaxResultBytes := flag.Int("plugin-max-result-bytes", 0, "nested external result byte cap")
	maxConcurrency := flag.Int("max-concurrency", 0, "maximum concurrent tool calls; 0 = host-bounded")
	var manifests stringsFlag
	flag.Var(&manifests, "plugin", "explicit nested external provider manifest (repeatable)")
	flag.Parse()

	if *workspace == "" {
		fmt.Fprintln(os.Stderr, "pons-hands: --workspace is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host, err := toolhost.New(toolhost.Config{
		Workspace:           *workspace,
		FSReadBytes:         *fsReadBytes,
		BashTimeout:         *bashTimeout,
		BashMaxLines:        *bashMaxLines,
		BashMaxBytes:        *bashMaxBytes,
		ExternalManifests:   manifests,
		ExternalPath:        *pluginPath,
		ExternalResultBytes: *pluginMaxResultBytes,
		ExternalCallTimeout: 60 * time.Second,
		MaxConcurrency:      *maxConcurrency,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pons-hands: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := host.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "pons-hands: close: %v\n", err)
		}
	}()

	if err := host.Serve(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "pons-hands: serve: %v\n", err)
		os.Exit(1)
	}
}
