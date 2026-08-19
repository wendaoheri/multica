package releasecompat

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var pinnedImagePattern = regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`)

type composeDocument struct {
	Services map[string]struct {
		Image  string            `json:"image"`
		Labels map[string]string `json:"labels"`
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
	b, err := os.ReadFile(composePath)
	if err != nil {
		return DeploymentIdentity{}, err
	}
	var doc composeDocument
	if err := decodeStrict(b, &doc); err != nil {
		return DeploymentIdentity{}, fmt.Errorf("expanded compose: %w", err)
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

func WriteDeploymentIdentity(path string, id DeploymentIdentity) error {
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
