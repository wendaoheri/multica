package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/deployment"
	"github.com/multica-ai/multica/server/internal/releasecompat"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if os.Args[1] == "verify-manifest" {
		verifyManifest(os.Args[2:])
		return
	}
	if os.Args[1] == "generate-manifest" {
		generateManifest(os.Args[2:])
		return
	}
	if os.Args[1] == "verify-deployment" {
		verifyDeployment(os.Args[2:])
		return
	}
	if os.Args[1] == "checksum" {
		fs := flag.NewFlagSet("checksum", flag.ExitOnError)
		path := fs.String("file", "", "file to hash")
		_ = fs.Parse(os.Args[2:])
		if *path == "" {
			fatal(errors.New("--file is required"))
		}
		sum, err := releasecompat.FileSHA256(*path)
		if err != nil {
			fatal(err)
		}
		fmt.Println(sum)
		return
	}
	if os.Args[1] == "record-migrator-execution" {
		recordMigratorExecution(os.Args[2:])
		return
	}
	if os.Args[1] == "begin-migrator-attempt" {
		beginMigratorAttempt(os.Args[2:])
		return
	}
	if os.Args[1] == "verify-migrator-execution" {
		verifyMigratorExecution(os.Args[2:])
		return
	}
	switch os.Args[1] {
	case "status", "advance-generation", "admission-close", "activate", "drain", "complete-drain":
	default:
		usage()
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fatal(errors.New("DATABASE_URL is required"))
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()
	store := deployment.NewControlStore(pool)

	switch os.Args[1] {
	case "status":
		snapshot, err := store.Load(context.Background())
		if err != nil {
			fatal(err)
		}
		printJSON(snapshot)
	case "advance-generation":
		expected := requiredGeneration(os.Args[2:])
		snapshot, err := store.AdvanceGeneration(context.Background(), expected)
		if err != nil {
			fatal(err)
		}
		printJSON(snapshot)
	case "admission-close":
		generation := requiredGeneration(os.Args[2:])
		if err := store.CloseAdmission(context.Background(), generation); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"generation": generation, "admission_open": false})
	case "activate":
		fs := flag.NewFlagSet("activate", flag.ExitOnError)
		generation := fs.Int64("generation", 0, "expected active generation")
		owner := fs.String("owner", "", "worker instance owner id")
		_ = fs.Parse(os.Args[2:])
		if *generation <= 0 || *owner == "" {
			fatal(errors.New("--generation and --owner are required"))
		}
		if err := store.Activate(context.Background(), *generation, *owner); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"generation": *generation, "owner": *owner, "claims_enabled": true, "admission_open": true})
	case "drain":
		fs := flag.NewFlagSet("drain", flag.ExitOnError)
		generation := fs.Int64("generation", 0, "expected active generation")
		owner := fs.String("owner", "", "worker owner id; empty drains an unowned claims-disabled generation")
		_ = fs.Parse(os.Args[2:])
		if *generation <= 0 {
			fatal(errors.New("--generation is required"))
		}
		if err := store.Drain(context.Background(), *generation, *owner); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"generation": *generation, "claims_enabled": false})
	case "complete-drain":
		fs := flag.NewFlagSet("complete-drain", flag.ExitOnError)
		generation := fs.Int64("generation", 0, "expected active generation")
		owner := fs.String("owner", "", "worker owner id")
		window := fs.Duration("observation-window", deployment.DrainObservationWindow, "continuous zero-activity observation window")
		_ = fs.Parse(os.Args[2:])
		if *generation <= 0 || *owner == "" {
			fatal(errors.New("--generation and --owner are required"))
		}
		if err := store.CompleteDrain(context.Background(), *generation, *owner, *window); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"generation": *generation, "owner": *owner, "status": "drained_owner_released"})
	default:
		usage()
	}
}

func recordMigratorExecution(args []string) {
	fs := flag.NewFlagSet("record-migrator-execution", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "release manifest JSON")
	artifactDir := fs.String("artifact-dir", "", "smoke artifact directory")
	combination := fs.String("combination", "", "actual W/K/S combination")
	identity := fs.String("deployment-identity", "", "verified deployment identity JSON")
	config := fs.String("config-file", "", "actual effective config")
	flags := fs.String("feature-flags-file", "", "actual effective flags")
	currentAttempt := fs.String("current-attempt", "", "durable current migrator attempt")
	output := fs.String("output", "", "durable one-shot execution record")
	_ = fs.Parse(args)
	if *manifestPath == "" || *combination == "" || *identity == "" || *config == "" || *flags == "" || *currentAttempt == "" || *output == "" {
		fatal(errors.New("all record-migrator-execution flags are required"))
	}
	if *artifactDir == "" {
		*artifactDir = filepath.Dir(*manifestPath)
	}
	manifest, err := releasecompat.Load(*manifestPath)
	if err != nil {
		fatal(err)
	}
	if err := releasecompat.RecordMigratorExecution(manifest, *artifactDir, *combination, *identity, *config, *flags, *currentAttempt, *output, time.Now()); err != nil {
		fatal(err)
	}
	printJSON(map[string]any{"status": "recorded", "release_id": manifest.ReleaseID, "combination": *combination})
}

func beginMigratorAttempt(args []string) {
	fs := flag.NewFlagSet("begin-migrator-attempt", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "release manifest JSON")
	artifactDir := fs.String("artifact-dir", "", "smoke artifact directory")
	combination := fs.String("combination", "", "actual W/K/S combination")
	identity := fs.String("deployment-identity", "", "verified deployment identity JSON")
	config := fs.String("config-file", "", "actual effective config")
	flags := fs.String("feature-flags-file", "", "actual effective flags")
	output := fs.String("output", "", "durable current migrator attempt")
	_ = fs.Parse(args)
	if *manifestPath == "" || *combination == "" || *identity == "" || *config == "" || *flags == "" || *output == "" {
		fatal(errors.New("all begin-migrator-attempt flags are required"))
	}
	if *artifactDir == "" {
		*artifactDir = filepath.Dir(*manifestPath)
	}
	manifest, err := releasecompat.Load(*manifestPath)
	if err != nil {
		fatal(err)
	}
	attempt, err := releasecompat.BeginMigratorAttempt(manifest, *artifactDir, *combination, *identity, *config, *flags, *output, time.Now())
	if err != nil {
		fatal(err)
	}
	printJSON(map[string]any{"status": "started", "release_id": manifest.ReleaseID, "combination": *combination, "attempt_id": attempt.AttemptID})
}

func verifyMigratorExecution(args []string) {
	fs := flag.NewFlagSet("verify-migrator-execution", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "release manifest JSON")
	artifactDir := fs.String("artifact-dir", "", "smoke artifact directory")
	combination := fs.String("combination", "", "actual W/K/S combination")
	currentAttempt := fs.String("current-attempt", "", "durable current migrator attempt")
	execution := fs.String("execution-record", "", "durable successful execution record")
	_ = fs.Parse(args)
	if *manifestPath == "" || *combination == "" || *currentAttempt == "" || *execution == "" {
		fatal(errors.New("all verify-migrator-execution flags are required"))
	}
	if *artifactDir == "" {
		*artifactDir = filepath.Dir(*manifestPath)
	}
	manifest, err := releasecompat.Load(*manifestPath)
	if err != nil {
		fatal(err)
	}
	record, err := releasecompat.VerifyMigratorExecution(manifest, *artifactDir, *combination, *currentAttempt, *execution)
	if err != nil {
		fatal(err)
	}
	printJSON(map[string]any{"status": "verified", "release_id": manifest.ReleaseID, "combination": *combination, "attempt_id": record.AttemptID, "deployment": record.Deployment})
}

func verifyDeployment(args []string) {
	fs := flag.NewFlagSet("verify-deployment", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "release manifest JSON")
	artifactDir := fs.String("artifact-dir", "", "smoke artifact directory")
	combination := fs.String("combination", "", "actual W/K/S combination")
	compose := fs.String("compose-json", "", "actual docker compose config --format json output")
	config := fs.String("config-file", "", "actual effective config")
	flags := fs.String("feature-flags-file", "", "actual effective flags")
	output := fs.String("output", "", "write verified actual identity JSON")
	_ = fs.Parse(args)
	if *manifestPath == "" || *combination == "" || *compose == "" || *config == "" || *flags == "" {
		fatal(errors.New("--manifest, --combination, --compose-json, --config-file, and --feature-flags-file are required"))
	}
	if *artifactDir == "" {
		*artifactDir = filepath.Dir(*manifestPath)
	}
	manifest, err := releasecompat.Load(*manifestPath)
	if err != nil {
		fatal(err)
	}
	id, err := releasecompat.VerifyDeployment(manifest, *artifactDir, *combination, *compose, *config, *flags)
	if err != nil {
		fatal(err)
	}
	if *output != "" {
		if err := releasecompat.WriteDeploymentIdentity(*output, id); err != nil {
			fatal(err)
		}
	}
	printJSON(map[string]any{"status": "verified", "release_id": manifest.ReleaseID, "combination": *combination, "deployment": id})
}

func requiredGeneration(args []string) int64 {
	fs := flag.NewFlagSet("generation", flag.ExitOnError)
	generation := fs.Int64("generation", 0, "expected active generation")
	_ = fs.Parse(args)
	if *generation <= 0 {
		fatal(errors.New("--generation must be positive"))
	}
	return *generation
}

func verifyManifest(args []string) {
	fs := flag.NewFlagSet("verify-manifest", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "release manifest JSON")
	artifactDir := fs.String("artifact-dir", "", "immutable smoke artifact directory")
	migrationDir := fs.String("migration-dir", "", "migrator artifact migration directory")
	checkDatabase := fs.Bool("check-database", false, "compare applied/pending state with schema_migrations")
	requireCombination := fs.String("require-combination", "", "require this exact W/K/S target to be ALLOW")
	_ = fs.Parse(args)
	if *manifestPath == "" {
		fatal(errors.New("--manifest is required"))
	}
	if *artifactDir == "" {
		*artifactDir = filepath.Dir(*manifestPath)
	}
	manifest, err := releasecompat.Load(*manifestPath)
	if err != nil {
		fatal(err)
	}
	if *requireCombination != "" {
		if err := releasecompat.ValidateTarget(manifest, *artifactDir, *requireCombination); err != nil {
			fatal(err)
		}
	} else if err := releasecompat.Validate(manifest, *artifactDir); err != nil {
		fatal(err)
	}
	if *migrationDir != "" {
		applied := map[string]bool{}
		if *checkDatabase {
			dbURL := os.Getenv("DATABASE_URL")
			if dbURL == "" {
				fatal(errors.New("DATABASE_URL is required with --check-database"))
			}
			pool, err := pgxpool.New(context.Background(), dbURL)
			if err != nil {
				fatal(err)
			}
			defer pool.Close()
			rows, err := pool.Query(context.Background(), "SELECT version FROM schema_migrations ORDER BY version")
			if err != nil {
				fatal(err)
			}
			for rows.Next() {
				var version string
				if err := rows.Scan(&version); err != nil {
					rows.Close()
					fatal(err)
				}
				applied[version] = true
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				fatal(err)
			}
			rows.Close()
		}
		if err := releasecompat.VerifyMigrations(manifest, *migrationDir, applied); err != nil {
			fatal(err)
		}
	}
	printJSON(map[string]any{"status": "verified", "release_id": manifest.ReleaseID, "required_combination": *requireCombination})
}

func generateManifest(args []string) {
	fs := flag.NewFlagSet("generate-manifest", flag.ExitOnError)
	releaseID := fs.String("release-id", "", "release identifier")
	fromDigest := fs.String("from-digest", "", "current backend digest")
	webDigest := fs.String("web-digest", "", "candidate web/backend digest")
	workerDigest := fs.String("worker-digest", "", "candidate worker digest")
	configFile := fs.String("config-file", "", "rendered non-secret config artifact")
	flagsFile := fs.String("feature-flags-file", "", "feature flag rules")
	rollbackScript := fs.String("rollback-script", "", "rollback script")
	migrationDir := fs.String("migration-dir", "", "migration artifact directory")
	classesFile := fs.String("migration-classes", "", "JSON object mapping migration filenames to none/expand/contract")
	output := fs.String("output", "manifest.json", "output path")
	_ = fs.Parse(args)
	for name, value := range map[string]string{
		"release-id": *releaseID, "from-digest": *fromDigest, "web-digest": *webDigest,
		"worker-digest": *workerDigest, "config-file": *configFile,
		"feature-flags-file": *flagsFile, "rollback-script": *rollbackScript,
		"migration-dir": *migrationDir, "migration-classes": *classesFile,
	} {
		if value == "" {
			fatal(fmt.Errorf("--%s is required", name))
		}
	}
	classesBytes, err := os.ReadFile(*classesFile)
	if err != nil {
		fatal(err)
	}
	classes := map[string]string{}
	if err := json.Unmarshal(classesBytes, &classes); err != nil {
		fatal(err)
	}
	applied := readAppliedMigrations()
	manifest, err := releasecompat.Generate(releasecompat.GenerateOptions{
		ReleaseID: *releaseID, FromDigest: *fromDigest, WebDigest: *webDigest,
		WorkerDigest: *workerDigest, ConfigFile: *configFile, FeatureFlagsFile: *flagsFile,
		RollbackScript: *rollbackScript, MigrationDir: *migrationDir,
		MigrationClasses: classes, Applied: applied,
	})
	if err != nil {
		fatal(err)
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fatal(err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(*output, b, 0o600); err != nil {
		fatal(err)
	}
	printJSON(map[string]string{"status": "generated_deny_by_default", "path": *output})
}

func readAppliedMigrations() map[string]bool {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fatal(errors.New("DATABASE_URL is required to generate a migration manifest"))
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()
	rows, err := pool.Query(context.Background(), "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		fatal(err)
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			fatal(err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		fatal(err)
	}
	return applied
}

func printJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "releasectl:", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: releasectl <status|advance-generation|admission-close|activate|drain|complete-drain|generate-manifest|verify-manifest|verify-deployment|begin-migrator-attempt|record-migrator-execution|verify-migrator-execution|checksum>")
	os.Exit(2)
}
