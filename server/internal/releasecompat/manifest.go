package releasecompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	DecisionAllow = "ALLOW"
	DecisionDeny  = "DENY"
)

var digestPattern = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`)

type Migration struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Class  string `json:"class"`
	State  string `json:"state"`
}

type Combination struct {
	Decision     string `json:"decision"`
	Report       string `json:"report,omitempty"`
	ReportSHA256 string `json:"report_sha256,omitempty"`
}

type Manifest struct {
	ReleaseID                 string                 `json:"release_id"`
	FromDigest                string                 `json:"from_digest"`
	WebDigest                 string                 `json:"web_digest"`
	WorkerDigest              string                 `json:"worker_digest"`
	ConfigChecksum            string                 `json:"config_checksum"`
	FeatureFlagsChecksum      string                 `json:"feature_flags_checksum"`
	MigrationManifestChecksum string                 `json:"migration_manifest_checksum"`
	Migrations                []Migration            `json:"migrations"`
	Combinations              map[string]Combination `json:"combinations"`
	RollbackScriptChecksum    string                 `json:"rollback_script_checksum"`
}

type GenerateOptions struct {
	ReleaseID        string
	FromDigest       string
	WebDigest        string
	WorkerDigest     string
	ConfigFile       string
	FeatureFlagsFile string
	RollbackScript   string
	MigrationDir     string
	MigrationClasses map[string]string
	Applied          map[string]bool
}

var RequiredCombinations = func() []string {
	var result []string
	for _, w := range []string{"W0", "W1"} {
		for _, k := range []string{"K0", "K1"} {
			for _, s := range []string{"S0", "S1", "S2"} {
				result = append(result, w+k+s)
			}
		}
	}
	return result
}()

func Generate(opts GenerateOptions) (Manifest, error) {
	configSum, err := fileSHA256(opts.ConfigFile)
	if err != nil {
		return Manifest{}, fmt.Errorf("config checksum: %w", err)
	}
	flagsSum, err := fileSHA256(opts.FeatureFlagsFile)
	if err != nil {
		return Manifest{}, fmt.Errorf("feature flags checksum: %w", err)
	}
	rollbackSum, err := fileSHA256(opts.RollbackScript)
	if err != nil {
		return Manifest{}, fmt.Errorf("rollback checksum: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(opts.MigrationDir, "*.up.sql"))
	if err != nil {
		return Manifest{}, err
	}
	sort.Strings(files)
	migrations := make([]Migration, 0, len(files))
	for _, file := range files {
		name := filepath.Base(file)
		class, ok := opts.MigrationClasses[name]
		version := strings.TrimSuffix(name, ".up.sql")
		if !ok && opts.Applied[version] {
			class = "historical"
		} else if !ok {
			return Manifest{}, fmt.Errorf("pending migration class missing for %s", name)
		}
		sum, err := fileSHA256(file)
		if err != nil {
			return Manifest{}, err
		}
		state := "pending"
		if opts.Applied[version] {
			state = "applied"
		}
		migrations = append(migrations, Migration{File: name, SHA256: sum, Class: class, State: state})
	}
	canonical, err := json.Marshal(migrations)
	if err != nil {
		return Manifest{}, err
	}
	migrationSum := sha256.Sum256(canonical)
	combinations := map[string]Combination{}
	for _, key := range RequiredCombinations {
		combinations[key] = Combination{Decision: DecisionDeny}
	}
	return Manifest{
		ReleaseID: opts.ReleaseID, FromDigest: opts.FromDigest, WebDigest: opts.WebDigest,
		WorkerDigest: opts.WorkerDigest, ConfigChecksum: configSum, FeatureFlagsChecksum: flagsSum,
		MigrationManifestChecksum: hex.EncodeToString(migrationSum[:]), Migrations: migrations,
		Combinations: combinations, RollbackScriptChecksum: rollbackSum,
	}, nil
}

func Load(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Validate(manifest Manifest, artifactDir string) error {
	var problems []string
	if strings.TrimSpace(manifest.ReleaseID) == "" {
		problems = append(problems, "release_id is required")
	}
	for name, value := range map[string]string{
		"from_digest":                 manifest.FromDigest,
		"web_digest":                  manifest.WebDigest,
		"worker_digest":               manifest.WorkerDigest,
		"config_checksum":             manifest.ConfigChecksum,
		"feature_flags_checksum":      manifest.FeatureFlagsChecksum,
		"migration_manifest_checksum": manifest.MigrationManifestChecksum,
		"rollback_script_checksum":    manifest.RollbackScriptChecksum,
	} {
		if !digestPattern.MatchString(value) {
			problems = append(problems, name+" must be a SHA-256 digest")
		}
	}

	last := ""
	seen := map[string]bool{}
	for _, migration := range manifest.Migrations {
		if migration.File <= last {
			problems = append(problems, "migrations must be unique and ordered")
		}
		last = migration.File
		if seen[migration.File] {
			problems = append(problems, "duplicate migration "+migration.File)
		}
		seen[migration.File] = true
		if !digestPattern.MatchString(migration.SHA256) {
			problems = append(problems, migration.File+": invalid sha256")
		}
		if migration.State != "applied" && migration.State != "pending" {
			problems = append(problems, migration.File+": state must be applied or pending")
		}
		if migration.State == "pending" && migration.Class != "expand" && migration.Class != "none" {
			problems = append(problems, migration.File+": pending migration is not expand-only")
		}
	}

	for _, key := range RequiredCombinations {
		combination, ok := manifest.Combinations[key]
		if !ok {
			problems = append(problems, key+": missing combination (unknown is DENY)")
			continue
		}
		switch combination.Decision {
		case DecisionDeny:
			if combination.Report != "" || combination.ReportSHA256 != "" {
				problems = append(problems, key+": DENY must not carry a smoke report")
			}
		case DecisionAllow:
			if combination.Report == "" || !digestPattern.MatchString(combination.ReportSHA256) {
				problems = append(problems, key+": ALLOW requires report and report_sha256")
				continue
			}
			path := filepath.Join(artifactDir, filepath.Clean(combination.Report))
			if !strings.HasPrefix(path, filepath.Clean(artifactDir)+string(os.PathSeparator)) {
				problems = append(problems, key+": report escapes artifact directory")
				continue
			}
			got, err := fileSHA256(path)
			if err != nil {
				problems = append(problems, key+": "+err.Error())
			} else if got != strings.TrimPrefix(combination.ReportSHA256, "sha256:") {
				problems = append(problems, key+": smoke report checksum mismatch")
			}
		default:
			problems = append(problems, key+": decision must be ALLOW or DENY")
		}
	}
	for key := range manifest.Combinations {
		if !contains(RequiredCombinations, key) {
			problems = append(problems, key+": unknown combination")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// VerifyMigrations binds the manifest to the exact migrator artifact and its
// target database state. Extra, missing, modified, or misclassified files all
// fail closed before migrate up is allowed to run.
func VerifyMigrations(manifest Manifest, migrationDir string, applied map[string]bool) error {
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	if len(files) != len(manifest.Migrations) {
		return fmt.Errorf("migration count mismatch: artifact=%d manifest=%d", len(files), len(manifest.Migrations))
	}
	for i, file := range files {
		entry := manifest.Migrations[i]
		if filepath.Base(file) != entry.File {
			return fmt.Errorf("migration order mismatch at %d: artifact=%s manifest=%s", i, filepath.Base(file), entry.File)
		}
		got, err := fileSHA256(file)
		if err != nil {
			return err
		}
		if got != strings.TrimPrefix(entry.SHA256, "sha256:") {
			return fmt.Errorf("migration checksum mismatch: %s", entry.File)
		}
		version := strings.TrimSuffix(entry.File, ".up.sql")
		wantState := "pending"
		if applied[version] {
			wantState = "applied"
		}
		if entry.State != wantState {
			return fmt.Errorf("migration state mismatch for %s: database=%s manifest=%s", entry.File, wantState, entry.State)
		}
	}
	canonical, err := json.Marshal(manifest.Migrations)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	want := strings.TrimPrefix(manifest.MigrationManifestChecksum, "sha256:")
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("migration manifest checksum mismatch")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read report: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
