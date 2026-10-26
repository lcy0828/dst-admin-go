package gameupdate

import (
	"sort"
	"strings"
)

func normalizeReleaseInstallations(values []ReleaseInstallationSelection) ([]ReleaseInstallationSelection, error) {
	if len(values) > 100 {
		return nil, ErrReleaseInvalid
	}
	var result []ReleaseInstallationSelection
	seen := make(map[string]bool)
	for _, value := range values {
		value.TargetID, value.InstallationID = strings.TrimSpace(value.TargetID), strings.TrimSpace(value.InstallationID)
		if !releaseIdentityPattern.MatchString(value.TargetID) || !releaseIdentityPattern.MatchString(value.InstallationID) {
			return nil, ErrReleaseInvalid
		}
		key := releaseInstallationKey(value.TargetID, value.InstallationID)
		if !seen[key] {
			result = append(result, value)
			seen[key] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return releaseInstallationKey(result[i].TargetID, result[i].InstallationID) < releaseInstallationKey(result[j].TargetID, result[j].InstallationID)
	})
	return result, nil
}
