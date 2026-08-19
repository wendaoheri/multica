package releasecompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordMigratorExecutionSurvivesOneShotCleanup(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.env")
	flags := filepath.Join(dir, "flags.yaml")
	mustWrite(t, config, "DATABASE_URL=postgres://fixture\n")
	mustWrite(t, flags, "feature_a: true\n")
	configSum, _ := fileSHA256(config)
	flagsSum, _ := fileSHA256(flags)
	m := validManifest()
	m.ConfigChecksum, m.FeatureFlagsChecksum = configSum, flagsSum
	id := *allowCombination("smoke.json", testDigest).Deployment
	id.ConfigChecksum, id.FeatureFlagsChecksum = configSum, flagsSum
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: testDigest, Deployment: &id}
	// Record generation validates the target first; use a valid immutable report.
	report := validSmokeReport(m, "W1K1S1")
	b, _ := json.Marshal(report)
	mustWrite(t, filepath.Join(dir, "smoke.json"), string(b))
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", Deployment: &id}
	m.Combinations["W1K1S1"] = func() Combination {
		c := m.Combinations["W1K1S1"]
		c.ReportSHA256, _ = fileSHA256(filepath.Join(dir, "smoke.json"))
		return c
	}()
	identityPath := filepath.Join(dir, "deployment.actual.json")
	b, _ = json.Marshal(id)
	mustWrite(t, identityPath, string(b))
	recordPath := filepath.Join(dir, "migrator-execution.json")
	completed := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	if err := RecordMigratorExecution(m, dir, "W1K1S1", identityPath, config, flags, recordPath, completed); err != nil {
		t.Fatalf("RecordMigratorExecution: %v", err)
	}
	// The record is host-persisted and remains valid after the simulated one-shot files disappear.
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}
	var got MigratorExecutionRecord
	b, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Result != "PASS" || got.CompletedAt != completed.Format(time.RFC3339) || got.Deployment != id {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestRecordMigratorExecutionRejectsActualInputDrift(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.env")
	flags := filepath.Join(dir, "flags.yaml")
	mustWrite(t, config, "A=1\n")
	mustWrite(t, flags, "feature_a: true\n")
	m := validManifest()
	id := *allowCombination("smoke.json", testDigest).Deployment
	id.ConfigChecksum, _ = fileSHA256(config)
	id.FeatureFlagsChecksum, _ = fileSHA256(flags)
	m.ConfigChecksum, m.FeatureFlagsChecksum = id.ConfigChecksum, id.FeatureFlagsChecksum
	report := validSmokeReport(m, "W1K1S1")
	b, _ := json.Marshal(report)
	mustWrite(t, filepath.Join(dir, "smoke.json"), string(b))
	reportSum, _ := fileSHA256(filepath.Join(dir, "smoke.json"))
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: reportSum, Deployment: &id}
	b, _ = json.Marshal(id)
	identityPath := filepath.Join(dir, "deployment.actual.json")
	mustWrite(t, identityPath, string(b))
	mustWrite(t, config, "A=2\n")
	output := filepath.Join(dir, "record.json")
	if err := RecordMigratorExecution(m, dir, "W1K1S1", identityPath, config, flags, output, time.Now()); err == nil {
		t.Fatal("config drift accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("record created after rejected drift: %v", err)
	}
}
