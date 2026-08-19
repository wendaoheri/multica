package releasecompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyDeploymentBindsActualExpandedInputs(t *testing.T) {
	dir := t.TempDir()
	config, flags := filepath.Join(dir, "config.env"), filepath.Join(dir, "flags.yaml")
	configBody := "DATABASE_URL=postgres://fixture\nREDIS_URL=redis://fixture\nREALTIME_RELAY_MODE=sharded\nAPP_ENV=test\n"
	mustWrite(t, config, configBody)
	mustWrite(t, flags, "feature_a: true\n")
	configSum, _ := fileSHA256(config)
	flagsSum, _ := fileSHA256(flags)
	m := validManifest()
	m.ConfigChecksum, m.FeatureFlagsChecksum = configSum, flagsSum
	d := strings.Repeat("a", 64)
	id := DeploymentIdentity{
		WebImage: "registry/web@sha256:" + d, WorkerImage: "registry/worker@sha256:" + d,
		FrontendImage: "registry/frontend@sha256:" + d, MigratorImage: "registry/migrator@sha256:" + d,
		ConfigChecksum: configSum, FeatureFlagsChecksum: flagsSum, MigrationManifestChecksum: m.MigrationManifestChecksum,
	}
	report := validSmokeReport(m, "W1K1S1")
	reportPath := filepath.Join(dir, "smoke.json")
	b, _ := json.Marshal(report)
	if err := os.WriteFile(reportPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	reportSum, _ := fileSHA256(reportPath)
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: reportSum, Deployment: &id}
	labels := map[string]string{
		"com.multica.release-id": m.ReleaseID, "com.multica.combination": "W1K1S1",
		"com.multica.config-checksum": configSum, "com.multica.flags-checksum": flagsSum,
		"com.multica.migration-checksum": m.MigrationManifestChecksum,
	}
	backend := func(image string) map[string]any {
		return map[string]any{
			"image": image, "labels": labels,
			"environment": map[string]string{
				"DATABASE_URL": "postgres://fixture", "REDIS_URL": "redis://fixture",
				"REALTIME_RELAY_MODE": "sharded", "APP_ENV": "test",
				"MULTICA_FEATURE_FLAGS_FILE": "/run/multica-release/feature-flags.yaml",
			},
			"volumes": []map[string]any{
				{"type": "bind", "source": config, "target": "/run/multica-release/config.env", "read_only": true},
				{"type": "bind", "source": flags, "target": "/run/multica-release/feature-flags.yaml", "read_only": true},
			},
		}
	}
	services := map[string]any{
		"blue-web": backend(id.WebImage), "green-web": backend(id.WebImage),
		"blue-worker": backend(id.WorkerImage), "green-worker": backend(id.WorkerImage),
		"blue-frontend":  map[string]any{"image": id.FrontendImage, "labels": labels},
		"green-frontend": map[string]any{"image": id.FrontendImage, "labels": labels},
		"migrator":       backend(id.MigratorImage),
	}
	services["migrator"].(map[string]any)["profiles"] = []string{"migration"}
	compose := filepath.Join(dir, "compose.json")
	writeCompose := func() {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"services": services})
		if err := os.WriteFile(compose, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCompose()
	if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err != nil {
		t.Fatalf("VerifyDeployment: %v", err)
	}

	t.Run("tag image", func(t *testing.T) {
		old := services["blue-web"].(map[string]any)["image"]
		services["blue-web"].(map[string]any)["image"] = "registry/web:latest"
		defer func() { services["blue-web"].(map[string]any)["image"] = old; writeCompose() }()
		writeCompose()
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("tag accepted")
		}
	})
	t.Run("actual digest drift", func(t *testing.T) {
		old := services["green-web"].(map[string]any)["image"]
		services["green-web"].(map[string]any)["image"] = "registry/web@sha256:" + strings.Repeat("b", 64)
		defer func() { services["green-web"].(map[string]any)["image"] = old; writeCompose() }()
		writeCompose()
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("digest drift accepted")
		}
	})
	t.Run("config path mismatch", func(t *testing.T) {
		volumes := services["green-web"].(map[string]any)["volumes"].([]map[string]any)
		old := volumes[0]["source"]
		volumes[0]["source"] = filepath.Join(dir, "other.env")
		defer func() { volumes[0]["source"] = old; writeCompose() }()
		writeCompose()
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("config path mismatch accepted")
		}
	})
	t.Run("flags path mismatch", func(t *testing.T) {
		volumes := services["green-worker"].(map[string]any)["volumes"].([]map[string]any)
		old := volumes[1]["source"]
		volumes[1]["source"] = filepath.Join(dir, "other-flags.yaml")
		defer func() { volumes[1]["source"] = old; writeCompose() }()
		writeCompose()
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("flags path mismatch accepted")
		}
	})
	t.Run("expanded config value drift", func(t *testing.T) {
		environment := services["green-worker"].(map[string]any)["environment"].(map[string]string)
		old := environment["APP_ENV"]
		environment["APP_ENV"] = "drift"
		defer func() { environment["APP_ENV"] = old; writeCompose() }()
		writeCompose()
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("expanded config value drift accepted")
		}
	})
	t.Run("missing migration profile", func(t *testing.T) {
		services["migrator"].(map[string]any)["profiles"] = nil
		defer func() { services["migrator"].(map[string]any)["profiles"] = []string{"migration"}; writeCompose() }()
		writeCompose()
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("migrator without profile accepted")
		}
	})
	t.Run("config drift", func(t *testing.T) {
		mustWrite(t, config, configBody+"DRIFT=true\n")
		defer mustWrite(t, config, configBody)
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("config drift accepted")
		}
	})
	t.Run("flags drift", func(t *testing.T) {
		mustWrite(t, flags, "feature_a: false\n")
		defer mustWrite(t, flags, "feature_a: true\n")
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("flags drift accepted")
		}
	})
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
