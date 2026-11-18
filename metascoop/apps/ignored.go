package apps

// ignoredReleaseTags lists release tags whose artifacts are never downloaded or
// published to the F-Droid repository. Add a tag here when a release's artifact
// can't be published, e.g. because it exceeds GitHub's 100 MB file size limit.
var ignoredReleaseTags = map[string]struct{}{
	// F-Droid APK is 138 MB, which exceeds GitHub's 100 MB file size limit.
	"v2026.10.1-bwpm": {},
}

// IsIgnoredRelease reports whether the release with the given tag name should be skipped.
func IsIgnoredRelease(tagName string) bool {
	_, ok := ignoredReleaseTags[tagName]
	return ok
}
