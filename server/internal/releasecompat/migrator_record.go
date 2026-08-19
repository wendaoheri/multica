package releasecompat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const MigratorExecutionRecordVersion = 1

type MigratorExecutionRecord struct {
	Version     int                `json:"version"`
	Result      string             `json:"result"`
	ReleaseID   string             `json:"release_id"`
	Combination string             `json:"combination"`
	CompletedAt string             `json:"completed_at"`
	Deployment  DeploymentIdentity `json:"deployment"`
}

// RecordMigratorExecution emits the durable identity of a successfully
// completed one-shot migration. Callers invoke it only after migrate up exits
// zero; the atomic rename prevents smoke from accepting a partial record.
func RecordMigratorExecution(manifest Manifest, artifactDir, combination, identityPath, configPath, flagsPath, output string, now time.Time) error {
	if err := ValidateTarget(manifest, artifactDir, combination); err != nil {
		return err
	}
	b, err := os.ReadFile(identityPath)
	if err != nil {
		return err
	}
	var identity DeploymentIdentity
	if err := decodeStrict(b, &identity); err != nil {
		return fmt.Errorf("deployment identity: %w", err)
	}
	want := manifest.Combinations[combination].Deployment
	if want == nil || identity != *want {
		return fmt.Errorf("deployment identity does not match target combination")
	}
	configSum, err := fileSHA256(configPath)
	if err != nil {
		return err
	}
	flagsSum, err := fileSHA256(flagsPath)
	if err != nil {
		return err
	}
	if normalizeDigest(configSum) != normalizeDigest(identity.ConfigChecksum) || normalizeDigest(flagsSum) != normalizeDigest(identity.FeatureFlagsChecksum) {
		return fmt.Errorf("one-shot config or feature flags checksum mismatch")
	}
	record := MigratorExecutionRecord{Version: MigratorExecutionRecordVersion, Result: "PASS", ReleaseID: manifest.ReleaseID, Combination: combination, CompletedAt: now.UTC().Format(time.RFC3339), Deployment: identity}
	b, err = json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(output)
	tmp, err := os.CreateTemp(dir, ".migrator-execution-*")
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
	return os.Rename(tmpName, output)
}
