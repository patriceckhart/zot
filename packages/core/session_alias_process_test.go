package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestNamedSessionLockHelperProcess(t *testing.T) {
	mode := os.Getenv("ZOT_TEST_ALIAS_MODE")
	if mode == "" {
		return
	}
	s, _, _, err := OpenNamedSession(os.Getenv("ZOT_TEST_ALIAS_ROOT"), os.Getenv("ZOT_TEST_ALIAS_CWD"), "shared", "test", "test", "test")
	if mode == "busy" {
		if !errors.Is(err, ErrSessionInUse) {
			os.Exit(2)
		}
		os.Exit(0)
	}
	if err != nil {
		os.Exit(3)
	}
	if err := s.AppendMessage(aliasTestMessage()); err != nil {
		os.Exit(4)
	}
	// Deliberately omit Close, simulating an abrupt process exit.
	os.Exit(0)
}

func TestNamedSessionLocksAcrossProcessesAndRecoversAfterExit(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	s, _, _, err := OpenNamedSession(root, cwd, "shared", "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := s.ID
	if err := s.AppendMessage(aliasTestMessage()); err != nil {
		t.Fatal(err)
	}
	run := func(mode string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNamedSessionLockHelperProcess$")
		cmd.Env = append(os.Environ(), "ZOT_TEST_ALIAS_MODE="+mode, "ZOT_TEST_ALIAS_ROOT="+root, "ZOT_TEST_ALIAS_CWD="+cwd)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s helper: %v, %s", mode, err, output)
		}
	}
	run("busy")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	run("exit")
	recovered, msgs, fresh, err := OpenNamedSession(root, cwd, "shared", "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if fresh || recovered.ID != id || len(msgs) != 2 {
		t.Fatal("process exit lost identity, history, or writer lease")
	}
}
