package releasecompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	DecisionAllow = "ALLOW"
	DecisionDeny  = "DENY"
)

var digestPattern = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`)
var combinationPattern = regexp.MustCompile(`^W[01]K[01]S[012]$`)

const SmokeReportVersion = 1

// SmokeChecks is deliberately explicit: a report produced by an older or
// partial harness cannot silently become proof for a new release contract.
type SmokeChecks struct {
	AuthPermissions        bool `json:"auth_permissions"`
	IssueCommentCRUD       bool `json:"issue_comment_crud"`
	AttachmentLifecycle    bool `json:"attachment_lifecycle"`
	WebSocketCompatibility bool `json:"websocket_compatibility"`
	TaskLifecycle          bool `json:"task_lifecycle"`
	ConcurrentClaim        bool `json:"concurrent_claim"`
	SchedulerLease         bool `json:"scheduler_lease"`
	AutopilotState         bool `json:"autopilot_state"`
	WebhookFakeEndpoint    bool `json:"webhook_fake_endpoint"`
	PRRefreshFakeAPI       bool `json:"pr_refresh_fake_api"`
	HeartbeatSweeper       bool `json:"heartbeat_sweeper"`
	RelayCrossProcess      bool `json:"relay_cross_process"`
	CriticalWritePath      bool `json:"critical_write_path"`
	NoDuplicateSideEffects bool `json:"no_duplicate_side_effects"`
	ExternalNetworkBlocked bool `json:"external_network_blocked"`
}

// SmokeReport is the immutable, machine-checked proof attached to one exact
// W/K/S combination. It is not a free-form log file.
type SmokeReport struct {
	Version                   int         `json:"version"`
	ReleaseID                 string      `json:"release_id"`
	Combination               string      `json:"combination"`
	Result                    string      `json:"result"`
	WebDigest                 string      `json:"web_digest"`
	WorkerDigest              string      `json:"worker_digest"`
	ConfigChecksum            string      `json:"config_checksum"`
	FeatureFlagsChecksum      string      `json:"feature_flags_checksum"`
	MigrationManifestChecksum string      `json:"migration_manifest_checksum"`
	StartedAt                 string      `json:"started_at"`
	CompletedAt               string      `json:"completed_at"`
	Checks                    SmokeChecks `json:"checks"`
}

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
	if err := decodeStrict(b, &manifest); err != nil {
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
			} else if err := validateSmokeReport(path, key, manifest); err != nil {
				problems = append(problems, key+": "+err.Error())
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

// ValidateTarget is the release-time decision gate. Artifact validation alone
// may accept a deny-by-default manifest; an actual cutover may not.
func ValidateTarget(manifest Manifest, artifactDir, target string) error {
	if err := Validate(manifest, artifactDir); err != nil {
		return err
	}
	if !combinationPattern.MatchString(target) {
		return fmt.Errorf("target combination must match W[01]K[01]S[012]")
	}
	combination, ok := manifest.Combinations[target]
	if !ok || combination.Decision != DecisionAllow {
		return fmt.Errorf("target combination %s is not ALLOW", target)
	}
	return nil
}

func validateSmokeReport(path, key string, manifest Manifest) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read smoke report: %w", err)
	}
	var report SmokeReport
	if err := decodeStrict(b, &report); err != nil {
		return fmt.Errorf("invalid smoke report structure: %w", err)
	}
	if report.Version != SmokeReportVersion {
		return fmt.Errorf("smoke report version must be %d", SmokeReportVersion)
	}
	if report.ReleaseID != manifest.ReleaseID || report.Combination != key || report.Result != "PASS" {
		return fmt.Errorf("smoke report release, combination, or result mismatch")
	}
	bindings := map[string][2]string{
		"web_digest":                  {report.WebDigest, manifest.WebDigest},
		"worker_digest":               {report.WorkerDigest, manifest.WorkerDigest},
		"config_checksum":             {report.ConfigChecksum, manifest.ConfigChecksum},
		"feature_flags_checksum":      {report.FeatureFlagsChecksum, manifest.FeatureFlagsChecksum},
		"migration_manifest_checksum": {report.MigrationManifestChecksum, manifest.MigrationManifestChecksum},
	}
	for name, values := range bindings {
		if normalizeDigest(values[0]) != normalizeDigest(values[1]) {
			return fmt.Errorf("smoke report %s mismatch", name)
		}
	}
	started, err := time.Parse(time.RFC3339, report.StartedAt)
	if err != nil {
		return fmt.Errorf("smoke report started_at must be RFC3339")
	}
	completed, err := time.Parse(time.RFC3339, report.CompletedAt)
	if err != nil || completed.Before(started) {
		return fmt.Errorf("smoke report completed_at must be RFC3339 and not precede started_at")
	}
	checks := map[string]bool{
		"auth_permissions":          report.Checks.AuthPermissions,
		"issue_comment_crud":        report.Checks.IssueCommentCRUD,
		"attachment_lifecycle":      report.Checks.AttachmentLifecycle,
		"websocket_compatibility":   report.Checks.WebSocketCompatibility,
		"task_lifecycle":            report.Checks.TaskLifecycle,
		"concurrent_claim":          report.Checks.ConcurrentClaim,
		"scheduler_lease":           report.Checks.SchedulerLease,
		"autopilot_state":           report.Checks.AutopilotState,
		"webhook_fake_endpoint":     report.Checks.WebhookFakeEndpoint,
		"pr_refresh_fake_api":       report.Checks.PRRefreshFakeAPI,
		"heartbeat_sweeper":         report.Checks.HeartbeatSweeper,
		"relay_cross_process":       report.Checks.RelayCrossProcess,
		"critical_write_path":       report.Checks.CriticalWritePath,
		"no_duplicate_side_effects": report.Checks.NoDuplicateSideEffects,
		"external_network_blocked":  report.Checks.ExternalNetworkBlocked,
	}
	for name, passed := range checks {
		if !passed {
			return fmt.Errorf("smoke report required check %s did not pass", name)
		}
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
	knownVersions := make(map[string]bool, len(manifest.Migrations))
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
		knownVersions[version] = true
		wantState := "pending"
		if applied[version] {
			wantState = "applied"
		}
		if entry.State != wantState {
			return fmt.Errorf("migration state mismatch for %s: database=%s manifest=%s", entry.File, wantState, entry.State)
		}
	}
	for version := range applied {
		if !knownVersions[version] {
			return fmt.Errorf("database contains unknown migration version %s", version)
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

func decodeStrict(b []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON values are not allowed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("trailing JSON value is not allowed")
	}
	return nil
}

func normalizeDigest(value string) string { return strings.TrimPrefix(value, "sha256:") }

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
