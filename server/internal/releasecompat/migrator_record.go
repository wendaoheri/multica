package releasecompat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const (
	MigratorAttemptVersion         = 1
	MigratorExecutionRecordVersion = 1
)

var attemptIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type MigratorAttempt struct {
	Version     int                `json:"version"`
	AttemptID   string             `json:"attempt_id"`
	ReleaseID   string             `json:"release_id"`
	Combination string             `json:"combination"`
	StartedAt   string             `json:"started_at"`
	Deployment  DeploymentIdentity `json:"deployment"`
}

type MigratorExecutionRecord struct {
	Version     int                `json:"version"`
	Result      string             `json:"result"`
	AttemptID   string             `json:"attempt_id"`
	ReleaseID   string             `json:"release_id"`
	Combination string             `json:"combination"`
	StartedAt   string             `json:"started_at"`
	CompletedAt string             `json:"completed_at"`
	Deployment  DeploymentIdentity `json:"deployment"`
}

// BeginMigratorAttempt atomically replaces the current attempt before any
// verification or migration command is allowed to run. A later failure leaves
// this new nonce in place, so an older success record cannot authorize smoke.
func BeginMigratorAttempt(manifest Manifest, artifactDir, combination, identityPath, configPath, flagsPath, output string, now time.Time) (MigratorAttempt, error) {
	return beginMigratorAttempt(manifest, artifactDir, combination, identityPath, configPath, flagsPath, output, now, rand.Reader)
}

func beginMigratorAttempt(manifest Manifest, artifactDir, combination, identityPath, configPath, flagsPath, output string, now time.Time, random io.Reader) (MigratorAttempt, error) {
	identity, err := verifyMigratorInputs(manifest, artifactDir, combination, identityPath, configPath, flagsPath)
	if err != nil {
		return MigratorAttempt{}, err
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return MigratorAttempt{}, fmt.Errorf("generate migrator attempt id: %w", err)
	}
	attempt := MigratorAttempt{
		Version: MigratorAttemptVersion, AttemptID: hex.EncodeToString(nonce),
		ReleaseID: manifest.ReleaseID, Combination: combination,
		StartedAt: now.UTC().Format(time.RFC3339Nano), Deployment: identity,
	}
	if err := writeJSONAtomic(output, attempt, ".migrator-attempt-*"); err != nil {
		return MigratorAttempt{}, err
	}
	return attempt, nil
}

// RecordMigratorExecution completes exactly the current attempt. Callers invoke
// it only after migrate up exits zero. If this write fails, the current attempt
// remains unmatched and runtime identity stays fail closed.
func RecordMigratorExecution(manifest Manifest, artifactDir, combination, identityPath, configPath, flagsPath, currentAttemptPath, output string, now time.Time) error {
	identity, err := verifyMigratorInputs(manifest, artifactDir, combination, identityPath, configPath, flagsPath)
	if err != nil {
		return err
	}
	attempt, err := loadMigratorAttempt(currentAttemptPath)
	if err != nil {
		return err
	}
	started, err := validateAttempt(manifest, combination, attempt)
	if err != nil {
		return err
	}
	if attempt.Deployment != identity {
		return fmt.Errorf("current migrator attempt deployment identity mismatch")
	}
	completed := now.UTC()
	if completed.Before(started) {
		return fmt.Errorf("migrator completion precedes attempt start")
	}
	record := MigratorExecutionRecord{
		Version: MigratorExecutionRecordVersion, Result: "PASS",
		AttemptID: attempt.AttemptID, ReleaseID: attempt.ReleaseID,
		Combination: attempt.Combination, StartedAt: attempt.StartedAt,
		CompletedAt: completed.Format(time.RFC3339Nano), Deployment: attempt.Deployment,
	}
	return writeJSONAtomic(output, record, ".migrator-execution-*")
}

// VerifyMigratorExecution requires the durable current attempt and successful
// record to describe the same nonce and bindings. It deliberately has no age
// window or filesystem-mtime fallback.
func VerifyMigratorExecution(manifest Manifest, artifactDir, combination, currentAttemptPath, executionPath string) (MigratorExecutionRecord, error) {
	if err := ValidateTarget(manifest, artifactDir, combination); err != nil {
		return MigratorExecutionRecord{}, err
	}
	attempt, err := loadMigratorAttempt(currentAttemptPath)
	if err != nil {
		return MigratorExecutionRecord{}, err
	}
	started, err := validateAttempt(manifest, combination, attempt)
	if err != nil {
		return MigratorExecutionRecord{}, err
	}
	var record MigratorExecutionRecord
	if err := loadStrictJSON(executionPath, &record); err != nil {
		return MigratorExecutionRecord{}, fmt.Errorf("migrator execution record: %w", err)
	}
	if record.Version != MigratorExecutionRecordVersion || record.Result != "PASS" {
		return MigratorExecutionRecord{}, fmt.Errorf("migrator execution record is not a supported PASS record")
	}
	if !attemptIDPattern.MatchString(record.AttemptID) || record.AttemptID != attempt.AttemptID {
		return MigratorExecutionRecord{}, fmt.Errorf("migrator execution attempt id mismatch")
	}
	if record.ReleaseID != attempt.ReleaseID || record.Combination != attempt.Combination ||
		record.StartedAt != attempt.StartedAt || record.Deployment != attempt.Deployment {
		return MigratorExecutionRecord{}, fmt.Errorf("migrator execution record does not match current attempt")
	}
	recordStarted, err := parseStrictRFC3339("execution started_at", record.StartedAt)
	if err != nil {
		return MigratorExecutionRecord{}, err
	}
	completed, err := parseStrictRFC3339("execution completed_at", record.CompletedAt)
	if err != nil {
		return MigratorExecutionRecord{}, err
	}
	if !recordStarted.Equal(started) || completed.Before(started) {
		return MigratorExecutionRecord{}, fmt.Errorf("migrator execution timestamps are inconsistent")
	}
	return record, nil
}

func verifyMigratorInputs(manifest Manifest, artifactDir, combination, identityPath, configPath, flagsPath string) (DeploymentIdentity, error) {
	if err := ValidateTarget(manifest, artifactDir, combination); err != nil {
		return DeploymentIdentity{}, err
	}
	var identity DeploymentIdentity
	if err := loadStrictJSON(identityPath, &identity); err != nil {
		return DeploymentIdentity{}, fmt.Errorf("deployment identity: %w", err)
	}
	want := manifest.Combinations[combination].Deployment
	if want == nil || identity != *want {
		return DeploymentIdentity{}, fmt.Errorf("deployment identity does not match target combination")
	}
	configSum, err := fileSHA256(configPath)
	if err != nil {
		return DeploymentIdentity{}, err
	}
	flagsSum, err := fileSHA256(flagsPath)
	if err != nil {
		return DeploymentIdentity{}, err
	}
	if normalizeDigest(configSum) != normalizeDigest(identity.ConfigChecksum) || normalizeDigest(flagsSum) != normalizeDigest(identity.FeatureFlagsChecksum) {
		return DeploymentIdentity{}, fmt.Errorf("one-shot config or feature flags checksum mismatch")
	}
	return identity, nil
}

func loadMigratorAttempt(path string) (MigratorAttempt, error) {
	var attempt MigratorAttempt
	if err := loadStrictJSON(path, &attempt); err != nil {
		return MigratorAttempt{}, fmt.Errorf("current migrator attempt: %w", err)
	}
	return attempt, nil
}

func validateAttempt(manifest Manifest, combination string, attempt MigratorAttempt) (time.Time, error) {
	if attempt.Version != MigratorAttemptVersion || !attemptIDPattern.MatchString(attempt.AttemptID) {
		return time.Time{}, fmt.Errorf("current migrator attempt version or id is invalid")
	}
	want := manifest.Combinations[combination].Deployment
	if attempt.ReleaseID != manifest.ReleaseID || attempt.Combination != combination || want == nil || attempt.Deployment != *want {
		return time.Time{}, fmt.Errorf("current migrator attempt binding mismatch")
	}
	return parseStrictRFC3339("attempt started_at", attempt.StartedAt)
}

func parseStrictRFC3339(field, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	return parsed, nil
}

func loadStrictJSON(path string, target any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(b, target)
}

func writeJSONAtomic(path string, value any, pattern string) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Rename is the final fallible operation. Once it succeeds, callers must see
	// success; returning an error after installing a matching completion record
	// could let a failed command nevertheless authorize runtime smoke.
	return nil
}
