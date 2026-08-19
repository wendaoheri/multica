package releasecompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	m := validManifest()
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: sum}
	if err := Validate(m, dir); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: testDigest}
	if err := Validate(m, dir); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Validate mismatch error = %v", err)
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
