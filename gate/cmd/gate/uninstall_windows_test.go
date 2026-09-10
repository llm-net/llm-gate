package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests must run on Windows; cross compilation is not their acceptance.
func TestWindowsSelfRemovalProcess(t *testing.T) {
	if os.Getenv("GATE_TEST_SELF_REMOVAL") == "1" {
		path, err := os.Executable()
		if err != nil {
			os.Exit(10)
		}
		st, err := os.Stat(path)
		if err != nil {
			os.Exit(11)
		}
		hash, err := pathDigest(path)
		if err != nil {
			os.Exit(12)
		}
		finish, cancel, err := prepareSelfRemoval(&fileChange{path: path, before: st, hash: hash}, os.Stdout, os.Stderr)
		if err != nil {
			os.Exit(13)
		}
		if err := finish(); err != nil {
			cancel()
			os.Exit(14)
		}
		os.Exit(0)
	}
	dir := filepath.Join(t.TempDir(), "中文 gate & [x] $ (test)")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gate.exe")
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-test.run=^TestWindowsSelfRemovalProcess$")
	cmd.Env = envWith(os.Environ(), "GATE_TEST_SELF_REMOVAL", "1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("self removal process: %v\n%s", err, out)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("helper did not remove its parent executable: %s", out)
}

func windowsRemovalFixture(t *testing.T) (uninstallJob, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "gate.exe")
	uninstallWrite(t, path, "original fixture")
	id, err := windowsFileIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := pathDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	job := uninstallJob{Schema: 1, Target: path, Identity: id, Hash: hash}
	body, _ := json.Marshal(job)
	jobPath := filepath.Join(dir, "job.json")
	if err := atomicWrite(jobPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "finish.ps1")
	if err := atomicWrite(script, append([]byte{0xef, 0xbb, 0xbf}, uninstallFinishScript...), 0600); err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, filepath.Join(dir, "commit"), "commit")
	return job, jobPath, script
}

func TestWindowsFinishPreservesReplacement(t *testing.T) {
	job, jobPath, script := windowsRemovalFixture(t)
	if err := os.Rename(job.Target, job.Target+".old"); err != nil {
		t.Fatal(err)
	}
	uninstallWrite(t, job.Target, "new installation")
	host, err := systemPowerShell()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(host, "-NoProfile", "-NonInteractive", "-File", script, "-Job", jobPath, "-Retry").CombinedOutput()
	if err == nil {
		t.Fatalf("replacement should reject: %s", out)
	}
	if uninstallRead(t, job.Target) != "new installation" {
		t.Fatal("new installation was removed")
	}
	if !strings.Contains(uninstallRead(t, filepath.Join(filepath.Dir(jobPath), "result.txt")), "-Retry") {
		t.Fatal("failure lacks retry command")
	}
}

func TestWindowsFinishRetriesOccupiedFile(t *testing.T) {
	job, jobPath, script := windowsRemovalFixture(t)
	f, err := os.Open(job.Target)
	if err != nil {
		t.Fatal(err)
	}
	host, err := systemPowerShell()
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	run := func() ([]byte, error) {
		return exec.Command(host, "-NoProfile", "-NonInteractive", "-File", script, "-Job", jobPath, "-Retry").CombinedOutput()
	}
	out, err := run()
	f.Close()
	if err == nil {
		t.Fatalf("occupied file should reject: %s", out)
	}
	if out, err := run(); err != nil {
		t.Fatalf("retry after close failed: %v\n%s", err, out)
	}
	uninstallMissing(t, job.Target)
	uninstallMissing(t, jobPath)
}
