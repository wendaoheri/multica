package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
	case "admission-open", "admission-close":
		generation := requiredGeneration(os.Args[2:])
		if err := store.SetAdmission(context.Background(), generation, os.Args[1] == "admission-open"); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"generation": generation, "admission_open": os.Args[1] == "admission-open"})
	case "enable-claims":
		fs := flag.NewFlagSet("enable-claims", flag.ExitOnError)
		generation := fs.Int64("generation", 0, "expected active generation")
		owner := fs.String("owner", "", "worker instance owner id")
		_ = fs.Parse(os.Args[2:])
		if *generation <= 0 || *owner == "" {
			fatal(errors.New("--generation and --owner are required"))
		}
		if err := store.EnableClaims(context.Background(), *generation, *owner); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"generation": *generation, "owner": *owner, "claims_enabled": true})
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
	default:
		usage()
	}
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
	if err := releasecompat.Validate(manifest, *artifactDir); err != nil {
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
	printJSON(map[string]any{"status": "verified", "release_id": manifest.ReleaseID})
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
	fmt.Fprintln(os.Stderr, "usage: releasectl <status|advance-generation|admission-open|admission-close|enable-claims|drain|generate-manifest|verify-manifest>")
	os.Exit(2)
}
