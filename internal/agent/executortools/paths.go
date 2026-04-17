package executortools

import "path/filepath"

// resolvePath joins path against workDir if path is relative.
func resolvePath(workDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workDir, path)
}
