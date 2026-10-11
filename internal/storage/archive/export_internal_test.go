package archive

// SetBeforePublishHookForTesting installs a test hook called immediately before archive publication.
// The returned cleanup function restores the previous hook.
func SetBeforePublishHookForTesting(hook func(tmpPath string) error) func() {
	prev := testBeforePublishHook
	testBeforePublishHook = hook
	return func() {
		testBeforePublishHook = prev
	}
}

// ExportTempFile is an alias of exportTempFile exposed for testing.
type ExportTempFile = exportTempFile

// SetCreateTempFileForTesting installs a temporary file factory for testing real I/O error boundaries.
// The returned cleanup function restores the default factory.
func SetCreateTempFileForTesting(fn func(dir, pattern string) (exportTempFile, error)) func() {
	prev := createTempFile
	createTempFile = fn
	return func() {
		createTempFile = prev
	}
}

// SetLimitsForTesting configures total record payload and manifest byte limits for testing.
// The returned cleanup function restores the default limits.
func SetLimitsForTesting(maxRecords, maxManifest int64) func() {
	prevRecords := maxTotalRecordBytes
	prevManifest := maxManifestBytes
	maxTotalRecordBytes = maxRecords
	maxManifestBytes = maxManifest
	return func() {
		maxTotalRecordBytes = prevRecords
		maxManifestBytes = prevManifest
	}
}
