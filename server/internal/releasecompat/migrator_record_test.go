package releasecompat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type migratorFixture struct {
	dir, config, flags, identity, current, execution string
	manifest                                         Manifest
	deployment                                       DeploymentIdentity
}

func newMigratorFixture(t *testing.T) migratorFixture {
	t.Helper()
	dir := t.TempDir()
	f := migratorFixture{
		dir: dir, config: filepath.Join(dir, "config.env"), flags: filepath.Join(dir, "flags.yaml"),
		identity: filepath.Join(dir, "deployment.actual.json"), current: filepath.Join(dir, "current-attempt.json"),
		execution: filepath.Join(dir, "migrator-execution.json"), manifest: validManifest(),
	}
	mustWrite(t, f.config, "DATABASE_URL=postgres://fixture\n")
	mustWrite(t, f.flags, "feature_a: true\n")
	configSum, _ := fileSHA256(f.config)
	flagsSum, _ := fileSHA256(f.flags)
	f.manifest.ConfigChecksum, f.manifest.FeatureFlagsChecksum = configSum, flagsSum
	f.deployment = *allowCombination("smoke.json", testDigest).Deployment
	f.deployment.ConfigChecksum, f.deployment.FeatureFlagsChecksum = configSum, flagsSum
	report := validSmokeReport(f.manifest, "W1K1S1")
	b, _ := json.Marshal(report)
	mustWrite(t, filepath.Join(dir, "smoke.json"), string(b))
	reportSum, _ := fileSHA256(filepath.Join(dir, "smoke.json"))
	f.manifest.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: reportSum, Deployment: &f.deployment}
	b, _ = json.Marshal(f.deployment)
	mustWrite(t, f.identity, string(b))
	return f
}

func (f migratorFixture) begin(t *testing.T, now time.Time, fill byte) MigratorAttempt {
	t.Helper()
	attempt, err := beginMigratorAttempt(f.manifest, f.dir, "W1K1S1", f.identity, f.config, f.flags, f.current, now, bytes.NewReader(bytes.Repeat([]byte{fill}, 32)))
	if err != nil {
		t.Fatalf("BeginMigratorAttempt: %v", err)
	}
	return attempt
}

func (f migratorFixture) complete(t *testing.T, now time.Time) {
	t.Helper()
	if err := RecordMigratorExecution(f.manifest, f.dir, "W1K1S1", f.identity, f.config, f.flags, f.current, f.execution, now); err != nil {
		t.Fatalf("RecordMigratorExecution: %v", err)
	}
}

func TestMigratorAttemptInvalidatesOlderSuccessBeforeMigration(t *testing.T) {
	f := newMigratorFixture(t)
	startA := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	attemptA := f.begin(t, startA, 'a')
	f.complete(t, startA.Add(time.Minute))
	if _, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution); err != nil {
		t.Fatalf("attempt A should verify: %v", err)
	}

	// A new current attempt is persisted before verification/migrate up. A
	// simulated migration failure leaves record A on disk, but it is stale now.
	attemptB := f.begin(t, startA.Add(2*time.Minute), 'b')
	if attemptA.AttemptID == attemptB.AttemptID {
		t.Fatal("retry reused attempt id")
	}
	if _, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution); err == nil || !strings.Contains(err.Error(), "attempt id mismatch") {
		t.Fatalf("old success A authorized attempt B: %v", err)
	}
}

func TestMigratorCompletionWriteFailureKeepsNewAttemptFailClosed(t *testing.T) {
	f := newMigratorFixture(t)
	start := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	f.begin(t, start, 'a')
	f.complete(t, start.Add(time.Minute))
	f.begin(t, start.Add(2*time.Minute), 'b')

	badOutput := filepath.Join(f.dir, "missing", "migrator-execution.json")
	if err := RecordMigratorExecution(f.manifest, f.dir, "W1K1S1", f.identity, f.config, f.flags, f.current, badOutput, start.Add(3*time.Minute)); err == nil {
		t.Fatal("completion record write unexpectedly succeeded")
	}
	if _, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution); err == nil {
		t.Fatal("old success authorized after attempt B completion write failure")
	}
	mustWrite(t, f.execution, "{\n")
	if _, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution); err == nil {
		t.Fatal("incomplete attempt B record authorized")
	}
}

func TestVerifyMigratorExecutionRejectsNonceAndTimestampForgery(t *testing.T) {
	f := newMigratorFixture(t)
	start := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	f.begin(t, start, 'b')
	f.complete(t, start.Add(time.Minute))
	original, err := os.ReadFile(f.execution)
	if err != nil {
		t.Fatal(err)
	}
	var valid MigratorExecutionRecord
	if err := decodeStrict(original, &valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*MigratorExecutionRecord)
	}{
		{"different attempt id", func(r *MigratorExecutionRecord) { r.AttemptID = strings.Repeat("c", 64) }},
		{"completed at epoch", func(r *MigratorExecutionRecord) { r.CompletedAt = "1970-01-01T00:00:00Z" }},
		{"completed before start", func(r *MigratorExecutionRecord) { r.CompletedAt = "2026-08-19T03:59:59Z" }},
		{"invalid completed format", func(r *MigratorExecutionRecord) { r.CompletedAt = "2026-08-19 04:01:00" }},
		{"invalid started format", func(r *MigratorExecutionRecord) { r.StartedAt = "not-rfc3339" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			record := valid
			tc.mutate(&record)
			b, _ := json.Marshal(record)
			mustWrite(t, f.execution, string(b))
			if _, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution); err == nil {
				t.Fatal("forged execution record authorized")
			}
		})
	}
	t.Run("invalid current attempt started format", func(t *testing.T) {
		var attempt MigratorAttempt
		b, err := os.ReadFile(f.current)
		if err != nil {
			t.Fatal(err)
		}
		if err := decodeStrict(b, &attempt); err != nil {
			t.Fatal(err)
		}
		attempt.StartedAt = "not-rfc3339"
		record := valid
		record.StartedAt = attempt.StartedAt
		b, _ = json.Marshal(attempt)
		mustWrite(t, f.current, string(b))
		b, _ = json.Marshal(record)
		mustWrite(t, f.execution, string(b))
		if _, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution); err == nil {
			t.Fatal("invalid current attempt timestamp authorized")
		}
	})
}

func TestMigratorAttemptSuccessSurvivesOneShotCleanup(t *testing.T) {
	f := newMigratorFixture(t)
	start := time.Date(2026, 8, 19, 4, 0, 0, 123, time.UTC)
	attempt := f.begin(t, start, 'b')
	f.complete(t, start.Add(time.Minute))
	if err := os.Remove(f.identity); err != nil {
		t.Fatal(err)
	}
	record, err := VerifyMigratorExecution(f.manifest, f.dir, "W1K1S1", f.current, f.execution)
	if err != nil {
		t.Fatalf("VerifyMigratorExecution after cleanup: %v", err)
	}
	if record.AttemptID != attempt.AttemptID || record.StartedAt != attempt.StartedAt || record.Deployment != f.deployment {
		t.Fatalf("unexpected record: %+v", record)
	}
}

func TestBeginMigratorAttemptRejectsActualInputDriftWithoutReplacingCurrent(t *testing.T) {
	f := newMigratorFixture(t)
	start := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	attempt := f.begin(t, start, 'a')
	mustWrite(t, f.config, "DATABASE_URL=postgres://drift\n")
	if _, err := BeginMigratorAttempt(f.manifest, f.dir, "W1K1S1", f.identity, f.config, f.flags, f.current, start.Add(time.Minute)); err == nil {
		t.Fatal("config drift accepted")
	}
	current, err := loadMigratorAttempt(f.current)
	if err != nil {
		t.Fatal(err)
	}
	if current.AttemptID != attempt.AttemptID {
		t.Fatal("failed begin replaced current attempt")
	}
}
