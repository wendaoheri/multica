package releasecompat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var pinnedImagePattern = regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`)

type composeVolume struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type composeDocument struct {
	Services map[string]struct {
		Image       string            `json:"image"`
		Labels      map[string]string `json:"labels"`
		Environment map[string]string `json:"environment"`
		Profiles    []string          `json:"profiles"`
		Volumes     []composeVolume   `json:"volumes"`
	} `json:"services"`
}

func validateDeploymentIdentity(id DeploymentIdentity) error {
	for name, image := range map[string]string{"web": id.WebImage, "worker": id.WorkerImage, "frontend": id.FrontendImage, "migrator": id.MigratorImage} {
		if !pinnedImagePattern.MatchString(image) {
			return fmt.Errorf("%s image must be immutable @sha256", name)
		}
	}
	for name, sum := range map[string]string{"config": id.ConfigChecksum, "feature flags": id.FeatureFlagsChecksum, "migration manifest": id.MigrationManifestChecksum} {
		if !digestPattern.MatchString(sum) {
			return fmt.Errorf("%s checksum is invalid", name)
		}
	}
	return nil
}

// VerifyDeployment compares an ALLOW package with the real Compose expansion
// and byte-for-byte effective configuration inputs used by the release.
func VerifyDeployment(manifest Manifest, artifactDir, target, composePath, configPath, flagsPath string) (DeploymentIdentity, error) {
	if err := ValidateTarget(manifest, artifactDir, target); err != nil {
		return DeploymentIdentity{}, err
	}
	var b []byte
	var err error
	if composePath == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(composePath)
	}
	if err != nil {
		return DeploymentIdentity{}, err
	}
	var doc composeDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return DeploymentIdentity{}, fmt.Errorf("expanded compose: %w", err)
	}
	if len(doc.Services) != 7 {
		return DeploymentIdentity{}, fmt.Errorf("expanded compose must contain exactly 7 release services, got %d", len(doc.Services))
	}
	configValues, err := parseNormalizedEnv(configPath)
	if err != nil {
		return DeploymentIdentity{}, fmt.Errorf("actual config: %w", err)
	}
	for _, key := range []string{"DATABASE_URL", "REDIS_URL", "REALTIME_RELAY_MODE"} {
		if configValues[key] == "" {
			return DeploymentIdentity{}, fmt.Errorf("actual config is missing required key %s", key)
		}
	}
	if configValues["REALTIME_RELAY_MODE"] != "sharded" {
		return DeploymentIdentity{}, fmt.Errorf("actual config REALTIME_RELAY_MODE must be sharded")
	}
	for _, key := range []string{
		"MULTICA_AUTO_MIGRATE", "MULTICA_FEATURE_FLAGS_FILE", "MULTICA_PROCESS_ROLE",
		"MULTICA_RELEASE_GENERATION", "MULTICA_WORKER_OWNER", "MULTICA_WORKER_ADMIN_ADDR",
		"MULTICA_WORKER_ADMIN_TOKEN", "PORT", "PGOPTIONS",
	} {
		if _, ok := configValues[key]; ok {
			return DeploymentIdentity{}, fmt.Errorf("actual config key %s is deployment-controlled", key)
		}
	}
	configAbs, err := canonicalPath(configPath)
	if err != nil {
		return DeploymentIdentity{}, fmt.Errorf("actual config path: %w", err)
	}
	flagsAbs, err := canonicalPath(flagsPath)
	if err != nil {
		return DeploymentIdentity{}, fmt.Errorf("actual feature flags path: %w", err)
	}
	comb := manifest.Combinations[target]
	w, k := target[1], target[3]
	serviceImages := map[string]string{}
	for name, svc := range doc.Services {
		if !pinnedImagePattern.MatchString(svc.Image) {
			return DeploymentIdentity{}, fmt.Errorf("compose service %s image is not immutable: %s", name, svc.Image)
		}
		serviceImages[name] = svc.Image
		for label, want := range map[string]string{
			"com.multica.release-id": manifest.ReleaseID, "com.multica.combination": target,
			"com.multica.config-checksum": manifest.ConfigChecksum, "com.multica.flags-checksum": manifest.FeatureFlagsChecksum,
			"com.multica.migration-checksum": manifest.MigrationManifestChecksum,
		} {
			if normalizeDigest(svc.Labels[label]) != normalizeDigest(want) {
				return DeploymentIdentity{}, fmt.Errorf("compose service %s label %s mismatch", name, label)
			}
		}
	}
	required := []string{"blue-web", "green-web", "blue-worker", "green-worker", "blue-frontend", "green-frontend", "migrator"}
	for _, name := range required {
		if serviceImages[name] == "" {
			return DeploymentIdentity{}, fmt.Errorf("expanded compose missing service %s", name)
		}
	}
	if !contains(doc.Services["migrator"].Profiles, "migration") {
		return DeploymentIdentity{}, fmt.Errorf("expanded migrator must use migration profile")
	}
	for _, name := range []string{"blue-web", "green-web", "blue-worker", "green-worker", "migrator"} {
		svc := doc.Services[name]
		if !hasReadOnlyBind(svc.Volumes, configAbs, "/run/multica-release/config.env") || !hasReadOnlyBind(svc.Volumes, flagsAbs, "/run/multica-release/feature-flags.yaml") {
			return DeploymentIdentity{}, fmt.Errorf("compose service %s does not consume the verified config/flags paths", name)
		}
		if svc.Environment["MULTICA_FEATURE_FLAGS_FILE"] != "/run/multica-release/feature-flags.yaml" {
			return DeploymentIdentity{}, fmt.Errorf("compose service %s feature flags path mismatch", name)
		}
		for key, want := range configValues {
			if got, ok := svc.Environment[key]; !ok || got != want {
				return DeploymentIdentity{}, fmt.Errorf("compose service %s config key %s does not match normalized input", name, key)
			}
		}
	}
	actual := DeploymentIdentity{
		WebImage:      serviceImages[map[byte]string{'0': "blue-web", '1': "green-web"}[w]],
		WorkerImage:   serviceImages[map[byte]string{'0': "blue-worker", '1': "green-worker"}[k]],
		FrontendImage: serviceImages[map[byte]string{'0': "blue-frontend", '1': "green-frontend"}[w]],
		MigratorImage: serviceImages["migrator"], MigrationManifestChecksum: manifest.MigrationManifestChecksum,
	}
	actual.ConfigChecksum, err = fileSHA256(configPath)
	if err != nil {
		return DeploymentIdentity{}, fmt.Errorf("actual config: %w", err)
	}
	actual.FeatureFlagsChecksum, err = fileSHA256(flagsPath)
	if err != nil {
		return DeploymentIdentity{}, fmt.Errorf("actual feature flags: %w", err)
	}
	if actual != *comb.Deployment {
		return DeploymentIdentity{}, fmt.Errorf("actual deployment identity does not match ALLOW package")
	}
	if normalizeDigest(strings.Split(actual.WebImage, "@sha256:")[1]) != normalizeDigest(manifest.WebDigest) {
		return DeploymentIdentity{}, fmt.Errorf("actual web digest mismatch")
	}
	if normalizeDigest(strings.Split(actual.WorkerImage, "@sha256:")[1]) != normalizeDigest(manifest.WorkerDigest) {
		return DeploymentIdentity{}, fmt.Errorf("actual worker digest mismatch")
	}
	if normalizeDigest(actual.ConfigChecksum) != normalizeDigest(manifest.ConfigChecksum) || normalizeDigest(actual.FeatureFlagsChecksum) != normalizeDigest(manifest.FeatureFlagsChecksum) {
		return DeploymentIdentity{}, fmt.Errorf("actual config or flags drift")
	}
	return actual, nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func parseNormalizedEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		raw := strings.TrimSuffix(scanner.Text(), "\r")
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		key, value, ok := strings.Cut(raw, "=")
		if !ok || !envKeyPattern.MatchString(key) || strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") || strings.Contains(value, "${") {
			return nil, fmt.Errorf("line %d is not canonical KEY=VALUE", line)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate config key %s", key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	return filepath.Clean(abs), nil
}

func hasReadOnlyBind(volumes []composeVolume, source, target string) bool {
	for _, volume := range volumes {
		actual, _ := canonicalPath(volume.Source)
		if volume.Type == "bind" && actual == source && volume.Target == target && volume.ReadOnly {
			return true
		}
	}
	return false
}

func WriteDeploymentIdentity(path string, id DeploymentIdentity) error {
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
