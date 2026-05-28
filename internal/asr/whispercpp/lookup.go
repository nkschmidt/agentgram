package whispercpp

import (
	"os"
	"os/exec"
	"path/filepath"
)

// LookupBin tries to find the whisper-cli binary in the system.
// Returns an absolute path or "" if nothing was found.
func LookupBin() string {
	for _, name := range []string{"whisper-cli", "whisper-cpp", "whisper"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// LookupModel searches for the default ggml-base.bin model in typical
// locations for macOS and Linux. We cover both package name variants:
// whisper.cpp (dot, original name) and whisper-cpp (dash, in homebrew/dpkg).
func LookupModel() string {
	// Subdirectory name where models may live. Prefix is glued on below.
	dirNames := []string{
		"whisper-cpp/models",
		"whisper.cpp/models",
		"whisper-cpp",
		"whisper.cpp",
		"whisper",
	}
	// System prefixes:
	//   /opt/homebrew/share — macOS homebrew (Apple Silicon)
	//   /usr/local/share    — macOS homebrew (Intel) + cross-platform
	//   /usr/share          — Linux: dpkg/rpm install
	//   /usr/lib            — Linux: less common, but occurs
	systemPrefixes := []string{
		"/opt/homebrew/share",
		"/usr/local/share",
		"/usr/share",
		"/usr/lib",
	}

	var candidates []string
	for _, prefix := range systemPrefixes {
		for _, d := range dirNames {
			candidates = append(candidates, filepath.Join(prefix, d, "ggml-base.bin"))
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		// User-local paths:
		//   ~/.cache/...              — XDG_CACHE_HOME, Linux + macOS
		//   ~/.local/share/...        — XDG_DATA_HOME, Linux convention
		//   ~/Library/Application Support/... — macOS convention
		userDirs := []string{
			".cache/whisper-cpp",
			".cache/whisper.cpp",
			".cache/whisper",
			".local/share/whisper-cpp",
			".local/share/whisper.cpp",
			".local/share/whisper",
			"Library/Application Support/whisper-cpp",
			"Library/Application Support/whisper.cpp",
		}
		for _, d := range userDirs {
			candidates = append(candidates, filepath.Join(home, d, "ggml-base.bin"))
		}
	}

	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}
