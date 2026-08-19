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
	config, flags := filepath.Join(dir, "config"), filepath.Join(dir, "flags")
	_ = os.WriteFile(config, []byte("actual-config\n"), 0o600)
	_ = os.WriteFile(flags, []byte("actual-flags\n"), 0o600)
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
	_ = os.WriteFile(reportPath, b, 0o600)
	reportSum, _ := fileSHA256(reportPath)
	m.Combinations["W1K1S1"] = Combination{Decision: DecisionAllow, Report: "smoke.json", ReportSHA256: reportSum, Deployment: &id}
	services := map[string]any{}
	labels := map[string]string{"com.multica.release-id": m.ReleaseID, "com.multica.combination": "W1K1S1", "com.multica.config-checksum": configSum, "com.multica.flags-checksum": flagsSum, "com.multica.migration-checksum": m.MigrationManifestChecksum}
	for _, name := range []string{"blue-web", "green-web"} {
		services[name] = map[string]any{"image": id.WebImage, "labels": labels}
	}
	for _, name := range []string{"blue-worker", "green-worker"} {
		services[name] = map[string]any{"image": id.WorkerImage, "labels": labels}
	}
	for _, name := range []string{"blue-frontend", "green-frontend"} {
		services[name] = map[string]any{"image": id.FrontendImage, "labels": labels}
	}
	services["migrator"] = map[string]any{"image": id.MigratorImage, "labels": labels}
	compose := filepath.Join(dir, "compose.json")
	b, _ = json.Marshal(map[string]any{"services": services})
	_ = os.WriteFile(compose, b, 0o600)
	if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err != nil {
		t.Fatalf("VerifyDeployment: %v", err)
	}

	t.Run("tag image", func(t *testing.T) {
		services["blue-web"] = map[string]any{"image": "registry/web:latest", "labels": labels}
		b, _ := json.Marshal(map[string]any{"services": services})
		_ = os.WriteFile(compose, b, 0o600)
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("tag accepted")
		}
	})
	services["blue-web"] = map[string]any{"image": id.WebImage, "labels": labels}
	t.Run("actual digest drift", func(t *testing.T) {
		services["green-web"] = map[string]any{"image": "registry/web@sha256:" + strings.Repeat("b", 64), "labels": labels}
		b, _ := json.Marshal(map[string]any{"services": services})
		_ = os.WriteFile(compose, b, 0o600)
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("digest drift accepted")
		}
	})
	services["green-web"] = map[string]any{"image": id.WebImage, "labels": labels}
	b, _ = json.Marshal(map[string]any{"services": services})
	_ = os.WriteFile(compose, b, 0o600)
	t.Run("config drift", func(t *testing.T) {
		_ = os.WriteFile(config, []byte("drift"), 0o600)
		if _, err := VerifyDeployment(m, dir, "W1K1S1", compose, config, flags); err == nil {
			t.Fatal("config drift accepted")
		}
	})
}
