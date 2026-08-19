package releasecompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validManifest() Manifest {
	combinations := map[string]Combination{}
	for _, key := range RequiredCombinations {
		combinations[key] = Combination{Decision: DecisionDeny}
	}
	return Manifest{
		ReleaseID: "PER-376-test", FromDigest: testDigest, WebDigest: testDigest,
		WorkerDigest: testDigest, ConfigChecksum: testDigest, FeatureFlagsChecksum: testDigest,
		MigrationManifestChecksum: testDigest, RollbackScriptChecksum: testDigest,
		Migrations:   []Migration{{File: "319_example.up.sql", SHA256: testDigest, Class: "expand", State: "pending"}},
		Combinations: combinations,
	}
}

func TestValidateDefaultsUnknownCombinationToDeny(t *testing.T) {
	m := validManifest()
	delete(m.Combinations, "W0K1S1")
	err := Validate(m, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "W0K1S1: missing combination") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestValidateRejectsPendingContractMigration(t *testing.T) {
	m := validManifest()
	m.Migrations[0].Class = "contract"
	err := Validate(m, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not expand-only") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestValidateAllowRequiresImmutableMatchingReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smoke.json")
	m := validManifest()
	report := validSmokeReport(m, "W1K1S1")
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: sum}
	if err := Validate(m, dir); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := ValidateTarget(m, dir, "W1K1S1"); err != nil {
		t.Fatalf("ValidateTarget: %v", err)
	}
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: testDigest}
	if err := Validate(m, dir); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Validate mismatch error = %v", err)
	}
}

func TestValidateRejectsFreeFormAndMismatchedSmokeReports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smoke.json")
	m := validManifest()
	if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(path)
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: sum}
	if err := Validate(m, dir); err == nil || !strings.Contains(err.Error(), "invalid smoke report structure") {
		t.Fatalf("free-form report error = %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*SmokeReport)
		message string
	}{
		{"combination", func(r *SmokeReport) { r.Combination = "W0K0S0" }, "combination"},
		{"web digest", func(r *SmokeReport) { r.WebDigest = strings.Repeat("b", 64) }, "web_digest mismatch"},
		{"worker digest", func(r *SmokeReport) { r.WorkerDigest = strings.Repeat("b", 64) }, "worker_digest mismatch"},
		{"config", func(r *SmokeReport) { r.ConfigChecksum = strings.Repeat("b", 64) }, "config_checksum mismatch"},
		{"flags", func(r *SmokeReport) { r.FeatureFlagsChecksum = strings.Repeat("b", 64) }, "feature_flags_checksum mismatch"},
		{"migration", func(r *SmokeReport) { r.MigrationManifestChecksum = strings.Repeat("b", 64) }, "migration_manifest_checksum mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := validSmokeReport(m, "W1K1S1")
			tc.mutate(&report)
			b, _ := json.Marshal(report)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			sum, _ = fileSHA256(path)
			m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: sum}
			if err := Validate(m, dir); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("binding error = %v, want %q", err, tc.message)
			}
		})
	}
}

func TestValidateTargetRejectsAllDenyAndWrongCombination(t *testing.T) {
	m := validManifest()
	if err := ValidateTarget(m, t.TempDir(), "W1K1S1"); err == nil || !strings.Contains(err.Error(), "not ALLOW") {
		t.Fatalf("all-DENY target error = %v", err)
	}
	if err := ValidateTarget(m, t.TempDir(), "W9K1S1"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("invalid target error = %v", err)
	}
}

func TestVerifyMigrationsRejectsArtifactDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "319_example.up.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE example (id BIGINT);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileSum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	m := validManifest()
	m.Migrations[0].SHA256 = fileSum
	canonical, err := json.Marshal(m.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	m.MigrationManifestChecksum = hex.EncodeToString(sum[:])
	if err := VerifyMigrations(m, dir, map[string]bool{}); err != nil {
		t.Fatalf("VerifyMigrations: %v", err)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMigrations(m, dir, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("VerifyMigrations drift error = %v", err)
	}
}

func TestVerifyMigrationsRejectsUnknownDatabaseVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "319_example.up.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE example (id BIGINT);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := validManifest()
	m.Migrations[0].SHA256, _ = fileSHA256(path)
	canonical, _ := json.Marshal(m.Migrations)
	sum := sha256.Sum256(canonical)
	m.MigrationManifestChecksum = hex.EncodeToString(sum[:])
	err := VerifyMigrations(m, dir, map[string]bool{"000_unknown": true})
	if err == nil || !strings.Contains(err.Error(), "unknown migration version") {
		t.Fatalf("unknown database version error = %v", err)
	}
}

func validSmokeReport(m Manifest, combination string) SmokeReport {
	checks := SmokeChecks{
		AuthPermissions: true, IssueCommentCRUD: true, AttachmentLifecycle: true,
		WebSocketCompatibility: true, TaskLifecycle: true, ConcurrentClaim: true,
		SchedulerLease: true, AutopilotState: true, WebhookFakeEndpoint: true,
		PRRefreshFakeAPI: true, HeartbeatSweeper: true, RelayCrossProcess: true,
		CriticalWritePath: true, NoDuplicateSideEffects: true, ExternalNetworkBlocked: true,
	}
	return SmokeReport{
		Version: SmokeReportVersion, ReleaseID: m.ReleaseID, Combination: combination, Result: "PASS",
		WebDigest: m.WebDigest, WorkerDigest: m.WorkerDigest, ConfigChecksum: m.ConfigChecksum,
		FeatureFlagsChecksum: m.FeatureFlagsChecksum, MigrationManifestChecksum: m.MigrationManifestChecksum,
		StartedAt: time.Unix(1, 0).UTC().Format(time.RFC3339), CompletedAt: time.Unix(2, 0).UTC().Format(time.RFC3339),
		Checks: checks,
	}
}
