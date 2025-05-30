package kubevirtci

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver"

	"github.com/google/go-github/github"
	"github.com/sirupsen/logrus"
	"kubevirt.io/project-infra/robots/pkg/querier"
)

var SemVerRegex = regexp.MustCompile(`^[v]?([0-9]+)\.([0-9]+)\.([0-9]+)$`)
var ProviderFolderRegex = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(-[a-z0-9-]+)?$`)

func BumpMinorReleaseOfProvider(providerDir string, minors []*github.RepositoryRelease) error {
	// Update the latest three minor k8s versions
	for _, release := range minors {
		err := bumpRelease(providerDir, release)
		if err != nil {
			return err
		}
	}
	return nil
}

func bumpRelease(providerDir string, release *github.RepositoryRelease) error {
	r := querier.ParseReleaseFull(release)
	dirEntries, err := os.ReadDir(providerDir)
	if err != nil {
		logrus.Infof("Failed to get contents of %s", providerDir)
	}
	k8sVersion := fmt.Sprintf("%s.%s", r.Major, r.Minor)
	for _, entry := range dirEntries {
		if (isProviderDirectory(entry)) && (strings.Contains(entry.Name(), k8sVersion)) {
			dir := filepath.Join(providerDir, entry.Name())
			fullVersion := strings.TrimPrefix(*release.TagName, "v")
			err = os.WriteFile(filepath.Join(dir, "version"), []byte(fullVersion), os.ModePerm)
			if err != nil {
				return fmt.Errorf("Failed to bump the version file '%s': %v", filepath.Join(dir, "version"), err)
			}
			logrus.Infof("Version %s.%s updated to %v", r.Major, r.Minor, fullVersion)
		}
	}
	return nil
}

// Drop providers which run k8s versions which are not supported anymore
func DropUnsupportedProviders(providerDir, clusterUpDir string, supportedReleases []*github.RepositoryRelease) error {
	if len(supportedReleases) == 0 {
		return fmt.Errorf("No supported releases provided")
	}
	dirEntries, err := os.ReadDir(providerDir)
	if err != nil {
		return err
	}
	var supportedVersions []*semver.Version
	for _, r := range supportedReleases {
		release := semver.MustParse(r.GetTagName())
		supportedVersions = append(supportedVersions, release)
	}
	sort.Slice(supportedVersions, func(i, j int) bool {
		return supportedVersions[i].Compare(supportedVersions[j]) == 1
	})
	for _, entry := range dirEntries {
		if !isProviderDirectory(entry) {
			continue
		}
		if isSupportedProvider(entry, supportedReleases) {
			continue
		}
		version := semver.MustParse(entry.Name() + ".0")
		if version.Compare(supportedVersions[0]) == 1 {
			// future version, keep it
			continue
		}
		err = deleteProvider(providerDir, clusterUpDir, entry)
		if err != nil {
			return err
		}
	}
	return nil
}

func isProviderDirectory(entry os.DirEntry) bool {
	if !entry.IsDir() {
		return false
	}
	return ProviderFolderRegex.MatchString(entry.Name())
}

func isSupportedProvider(entry os.DirEntry, supportedReleases []*github.RepositoryRelease) bool {
	for _, supportedRelease := range supportedReleases {
		release := querier.ParseRelease(supportedRelease)
		if majorMinor(release) == entry.Name() {
			return true
		}
	}
	return false
}

func deleteProvider(providerDir, clusterUpDir string, entry os.DirEntry) error {
	err := os.RemoveAll(path.Join(providerDir, entry.Name()))
	if err != nil {
		return err
	}
	err = os.RemoveAll(path.Join(clusterUpDir, fmt.Sprintf("k8s-%s", entry.Name())))
	return err
}

// EnsureProviderExists will search for a predecessor provider, copy its content and set the version file accordingly
// If a provider already exists, it will do nothing.
func EnsureProviderExists(providerDir string, clusterUpDir string, release *github.RepositoryRelease) error {
	existing, err := ReadExistingProviders(providerDir)
	if err != nil {
		return err
	}
	semver := *querier.ParseReleaseFull(release)

	for _, rel := range existing {
		cmp := rel.CompareMajorMinor(&semver)
		if cmp > 0 {
			// not yet there
			continue
		} else if cmp == 0 {
			return nil
		}
		// First smaller existing provider. Copy the provider.
		sourceProviderDir := filepath.Join(providerDir, majorMinor(&rel))
		targetProviderDir := filepath.Join(providerDir, majorMinor(&semver))

		err = copyRecursive(sourceProviderDir, targetProviderDir)
		if err != nil {
			return err
		}
		logrus.Infof("Added provider %s.%s with version %v", semver.Major, semver.Minor, semver.String())

		err = bumpRelease(providerDir, release)
		if err != nil {
			return err
		}

		sourceClusterUpDir := filepath.Join(clusterUpDir, fmt.Sprintf("k8s-%s.%s", rel.Major, rel.Minor))
		targetClusterUpDir := filepath.Join(clusterUpDir, fmt.Sprintf("k8s-%s.%s", semver.Major, semver.Minor))

		err = copyRecursive(sourceClusterUpDir, targetClusterUpDir)
		if err != nil {
			return err
		}

		break
	}
	return nil
}

func majorMinor(release *querier.SemVer) string {
	return fmt.Sprintf("%s.%s", release.Major, release.Minor)
}

func copyRecursive(sourceDir string, targetDir string) error {
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
			return fmt.Errorf("source dir %q does not exist: %v", sourceDir, err)
		}
		// proper recursive copy of dirs is complicated, let `cp` do that.
		err := exec.Command("cp", "-a", sourceDir, targetDir).Run()
		if err != nil {
			return err
		}
	}
	return nil
}

func ReadExistingProviders(providerDir string) ([]querier.SemVer, error) {
	semvers := []querier.SemVer{}
	fileinfo, err := os.ReadDir(providerDir)
	if err != nil {
		return nil, err
	}
	for _, file := range fileinfo {
		if file.IsDir() {
			if ProviderFolderRegex.MatchString(file.Name()) {
				versionBytes, err := os.ReadFile(filepath.Join(providerDir, file.Name(), "version"))
				if os.IsNotExist(err) {
					continue
				} else if err != nil {
					return nil, err
				}
				version := strings.TrimSpace(string(versionBytes))
				if !SemVerRegex.MatchString(version) {
					return nil, fmt.Errorf("Version file contains unparsable content: %s", version)
				}
				matches := SemVerRegex.FindStringSubmatch(version)
				semvers = append(semvers, querier.SemVer{Major: matches[1], Minor: matches[2], Patch: matches[3]})
			}
		}
	}
	sort.Slice(semvers, func(i, j int) bool {
		return semvers[i].Compare(&semvers[j]) > 0
	})
	return semvers, nil
}
