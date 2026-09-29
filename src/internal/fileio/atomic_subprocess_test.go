package fileio

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	atomicOverwriteSubprocessMarker = "QUARRY_ATOMIC_OVERWRITE_SUBPROCESS"
	atomicOverwriteCrashMarker      = "QUARRY_ATOMIC_OVERWRITE_CRASH_SUBPROCESS"
)

const atomicOverwriteSubprocessTimeout = 60 * time.Second

type atomicOverwriteChild struct {
	id      string
	payload string
	cmd     *exec.Cmd
	output  bytes.Buffer
	done    chan error
	waited  bool
	waitErr error
}

func TestWriteFileAtomicKilledProcessLeavesClassifiableRecoveryPhases(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("journaled atomic overwrite is currently supported on Windows and Linux")
	}
	oldBytes := []byte("old generation")
	newBytes := []byte("new generation")
	tests := []struct {
		name           string
		phase          string
		wantAction     AtomicRecoveryAction
		wantNew        bool
		wantJournal    bool
		wantTemporary  bool
		wantBackup     bool
		wantOrphanTemp bool
	}{
		{name: "complete temp before journal", phase: "before-journal", wantAction: AtomicRecoveryNone, wantOrphanTemp: true},
		{name: "durable journal before backup", phase: "after-journal", wantAction: AtomicRecoveryResume, wantJournal: true, wantTemporary: true},
		{name: "linked backup before publish", phase: "before-publish", wantAction: AtomicRecoveryResume, wantJournal: true, wantTemporary: true, wantBackup: true},
		{name: "replacement before directory sync", phase: "after-publish", wantAction: AtomicRecoveryFinalize, wantNew: true, wantJournal: true, wantBackup: true},
		{name: "backup removal before directory sync", phase: "after-backup-removal", wantAction: AtomicRecoveryFinalize, wantNew: true, wantJournal: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			controlDir := t.TempDir()
			path := filepath.Join(dataDir, "settings.json")
			markerPath := filepath.Join(controlDir, "phase-reached")
			if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
				t.Fatal(err)
			}

			var output bytes.Buffer
			cmd := exec.Command(os.Args[0], "-test.run=^TestAtomicOverwriteCrashSubprocessHelper$")
			cmd.Env = append(os.Environ(),
				atomicOverwriteCrashMarker+"=1",
				"QUARRY_ATOMIC_OVERWRITE_PATH="+path,
				"QUARRY_ATOMIC_OVERWRITE_PAYLOAD="+string(newBytes),
				"QUARRY_ATOMIC_OVERWRITE_CRASH_PHASE="+test.phase,
				"QUARRY_ATOMIC_OVERWRITE_CRASH_MARKER="+markerPath,
			)
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited && cmd.Process != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			waitForAtomicSubprocessFile(t, markerPath)
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("kill overwrite subprocess at %s: %v\n%s", test.phase, err, output.String())
			}
			waitErr := cmd.Wait()
			waited = true
			if waitErr == nil {
				t.Fatalf("overwrite subprocess exited successfully instead of being killed at %s\n%s", test.phase, output.String())
			}

			wantDestination := oldBytes
			if test.wantNew {
				wantDestination = newBytes
			}
			assertOptionalRecoveryArtifact(t, path, wantDestination)
			state, err := InspectAtomicWriteRecovery(path)
			if err != nil {
				t.Fatalf("inspect killed phase %s: %v", test.phase, err)
			}
			if state.Action != test.wantAction || state.JournalPresent != test.wantJournal || state.Resolved {
				t.Fatalf("killed phase %s state = %#v, want action=%q journal=%v unresolved", test.phase, state, test.wantAction, test.wantJournal)
			}

			if test.wantOrphanTemp {
				orphan := findSingleAtomicOperationTemp(t, dataDir, filepath.Base(path)+defaultTempSuffix+".")
				assertOptionalRecoveryArtifact(t, orphan, newBytes)
			} else {
				if state.Temporary.Exists != test.wantTemporary || state.Backup.Exists != test.wantBackup {
					t.Fatalf("killed phase %s artifacts = temporary:%#v backup:%#v", test.phase, state.Temporary, state.Backup)
				}
				if test.wantTemporary {
					assertOptionalRecoveryArtifact(t, state.Temporary.Path, newBytes)
				}
				if test.wantBackup {
					assertOptionalRecoveryArtifact(t, state.Backup.Path, oldBytes)
				}
			}

			journalPath := path + atomicWriteJournalSuffix
			journalBefore, journalErr := os.ReadFile(journalPath)
			if test.wantJournal && journalErr != nil {
				t.Fatalf("read killed-phase journal: %v", journalErr)
			}
			if !test.wantJournal && !errors.Is(journalErr, os.ErrNotExist) {
				t.Fatalf("unexpected pre-journal artifact: %v", journalErr)
			}
			recovered, recoverErr := RecoverAtomicWrite(path)
			if test.wantJournal {
				if !errors.Is(recoverErr, ErrAtomicRecoveryNeedsInspection) || recovered.Action != test.wantAction {
					t.Fatalf("public recovery at %s = %#v, %v; want inspection-only %q", test.phase, recovered, recoverErr, test.wantAction)
				}
				assertOptionalRecoveryArtifact(t, journalPath, journalBefore)
			} else if recoverErr != nil || recovered.Action != AtomicRecoveryNone {
				t.Fatalf("pre-journal public recovery = %#v, %v; want no-op", recovered, recoverErr)
			}
			assertOptionalRecoveryArtifact(t, path, wantDestination)
		})
	}
}

func TestAtomicOverwriteCrashSubprocessHelper(t *testing.T) {
	if os.Getenv(atomicOverwriteCrashMarker) != "1" {
		return
	}
	path := os.Getenv("QUARRY_ATOMIC_OVERWRITE_PATH")
	payload := os.Getenv("QUARRY_ATOMIC_OVERWRITE_PAYLOAD")
	phase := os.Getenv("QUARRY_ATOMIC_OVERWRITE_CRASH_PHASE")
	markerPath := os.Getenv("QUARRY_ATOMIC_OVERWRITE_CRASH_MARKER")
	if path == "" || phase == "" || markerPath == "" {
		t.Fatal("atomic overwrite crash subprocess environment is incomplete")
	}

	crashAt := func(candidate string) {
		if phase != candidate {
			return
		}
		if err := os.WriteFile(markerPath, []byte(candidate), 0o600); err != nil {
			t.Fatal(err)
		}
		select {}
	}
	beforeAtomicOverwriteJournal = func() { crashAt("before-journal") }
	afterAtomicOverwriteJournal = func() { crashAt("after-journal") }
	beforeAtomicOverwritePublish = func() { crashAt("before-publish") }
	afterAtomicOverwritePublish = func() { crashAt("after-publish") }
	afterAtomicOverwriteBackupRemoval = func() { crashAt("after-backup-removal") }

	_, err := WriteFileAtomic(path, []byte(payload), AtomicWriteOptions{Overwrite: true})
	t.Fatalf("atomic overwrite returned before requested crash phase %q: %v", phase, err)
}

func findSingleAtomicOperationTemp(t *testing.T, dir string, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			matches = append(matches, filepath.Join(dir, entry.Name()))
		}
	}
	if len(matches) != 1 {
		t.Fatalf("operation temp matches = %v, want exactly one with prefix %q", matches, prefix)
	}
	return matches[0]
}

func TestWriteFileAtomicCrossProcessJournalOwnershipAllowsOneWriter(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("secure atomic journal publication is currently supported on Windows and Linux")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	syncDir := filepath.Join(dir, "sync")
	if err := os.Mkdir(syncDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old generation"), 0o600); err != nil {
		t.Fatal(err)
	}

	children := []*atomicOverwriteChild{
		{id: "a", payload: "generation a"},
		{id: "b", payload: "generation b"},
	}
	for _, child := range children {
		child.cmd = exec.Command(os.Args[0], "-test.run=^TestAtomicOverwriteSubprocessHelper$")
		child.cmd.Env = append(os.Environ(),
			atomicOverwriteSubprocessMarker+"=1",
			"QUARRY_ATOMIC_OVERWRITE_PATH="+path,
			"QUARRY_ATOMIC_OVERWRITE_SYNC_DIR="+syncDir,
			"QUARRY_ATOMIC_OVERWRITE_ID="+child.id,
			"QUARRY_ATOMIC_OVERWRITE_PAYLOAD="+child.payload,
		)
		child.cmd.Stdout = &child.output
		child.cmd.Stderr = &child.output
		if err := child.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		child.done = make(chan error, 1)
		go func(child *atomicOverwriteChild) {
			child.done <- child.cmd.Wait()
		}(child)
	}
	t.Cleanup(func() {
		for _, child := range children {
			if child.cmd.Process != nil {
				_ = child.cmd.Process.Kill()
			}
		}
	})

	for _, child := range children {
		waitForAtomicSubprocessFile(t, filepath.Join(syncDir, "ready-"+child.id))
	}

	if err := os.WriteFile(filepath.Join(syncDir, "release"), []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	winnerID := waitForAtomicSubprocessWinnerAndLoser(t, syncDir, children)
	if got, err := os.ReadFile(path); err != nil || string(got) != "old generation" {
		t.Fatalf("journal ownership phase changed the destination: %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "finish"), []byte("finish"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, child := range children {
		waitForAtomicSubprocessExit(t, child)
	}
	results := map[string]string{}
	for _, child := range children {
		result, err := os.ReadFile(filepath.Join(syncDir, "result-"+child.id))
		if err != nil {
			t.Fatal(err)
		}
		results[child.id] = string(result)
	}
	if results[winnerID] != "success" {
		t.Fatalf("journal owner %q result = %q, want success (all results %#v)", winnerID, results[winnerID], results)
	}
	loserID := "a"
	if winnerID == loserID {
		loserID = "b"
	}
	if results[loserID] != "blocked" {
		t.Fatalf("competing writer %q result = %q, want blocked (all results %#v)", loserID, results[loserID], results)
	}
	winnerPayload := children[0].payload
	if children[1].id == winnerID {
		winnerPayload = children[1].payload
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != winnerPayload {
		t.Fatalf("destination = %q, %v; want winning payload %q", got, err, winnerPayload)
	}
	if _, err := os.Lstat(path + atomicWriteJournalSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("winning operation journal remains: %v", err)
	}
}

func TestAtomicOverwriteSubprocessHelper(t *testing.T) {
	if os.Getenv(atomicOverwriteSubprocessMarker) != "1" {
		return
	}
	path := os.Getenv("QUARRY_ATOMIC_OVERWRITE_PATH")
	syncDir := os.Getenv("QUARRY_ATOMIC_OVERWRITE_SYNC_DIR")
	id := os.Getenv("QUARRY_ATOMIC_OVERWRITE_ID")
	payload := os.Getenv("QUARRY_ATOMIC_OVERWRITE_PAYLOAD")
	if path == "" || syncDir == "" || id == "" {
		t.Fatal("atomic overwrite subprocess environment is incomplete")
	}

	beforeAtomicOverwriteJournal = func() {
		if err := os.WriteFile(filepath.Join(syncDir, "ready-"+id), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForAtomicSubprocessFile(t, filepath.Join(syncDir, "release"))
	}
	afterAtomicOverwriteJournal = func() {
		if err := os.WriteFile(filepath.Join(syncDir, "claimed-"+id), []byte("claimed"), 0o600); err != nil {
			t.Fatal(err)
		}
		waitForAtomicSubprocessFile(t, filepath.Join(syncDir, "finish"))
	}

	_, err := WriteFileAtomic(path, []byte(payload), AtomicWriteOptions{Overwrite: true})
	result := "success"
	if errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		if !errors.Is(err, ErrExists) {
			t.Fatalf("journal ownership error did not preserve the no-clobber collision: %v", err)
		}
		result = "blocked"
	} else if err != nil {
		t.Fatalf("unexpected overwrite result: %v", err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "result-"+id), []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForAtomicSubprocessFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(atomicOverwriteSubprocessTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for subprocess marker %s", path)
}

func waitForAtomicSubprocessWinnerAndLoser(t *testing.T, syncDir string, children []*atomicOverwriteChild) string {
	t.Helper()
	deadline := time.Now().Add(atomicOverwriteSubprocessTimeout)
	for time.Now().Before(deadline) {
		winner := ""
		blocked := 0
		for _, child := range children {
			observeAtomicSubprocessExit(child)
			if child.waited && child.waitErr != nil {
				t.Fatalf("child %s failed before ownership was established: %v\n%s", child.id, child.waitErr, child.output.String())
			}
			if _, err := os.Lstat(filepath.Join(syncDir, "claimed-"+child.id)); err == nil {
				winner = child.id
			}
			if result, err := os.ReadFile(filepath.Join(syncDir, "result-"+child.id)); err == nil && string(result) == "blocked" {
				blocked++
			}
		}
		if winner != "" && blocked == 1 {
			return winner
		}
		time.Sleep(2 * time.Millisecond)
	}
	entries, readErr := os.ReadDir(syncDir)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	t.Fatal(fmt.Errorf("timed out waiting for one journal owner and one blocked writer; markers=%v, read error=%v", names, readErr))
	return ""
}

func observeAtomicSubprocessExit(child *atomicOverwriteChild) {
	if child == nil || child.waited || child.done == nil {
		return
	}
	select {
	case child.waitErr = <-child.done:
		child.waited = true
	default:
	}
}

func waitForAtomicSubprocessExit(t *testing.T, child *atomicOverwriteChild) {
	t.Helper()
	if child == nil {
		t.Fatal("atomic overwrite subprocess is nil")
	}
	observeAtomicSubprocessExit(child)
	if !child.waited {
		timer := time.NewTimer(atomicOverwriteSubprocessTimeout)
		defer timer.Stop()
		select {
		case child.waitErr = <-child.done:
			child.waited = true
		case <-timer.C:
			t.Fatalf("timed out waiting for child %s to exit\n%s", child.id, child.output.String())
		}
	}
	if child.waitErr != nil {
		t.Fatalf("child %s failed: %v\n%s", child.id, child.waitErr, child.output.String())
	}
}
