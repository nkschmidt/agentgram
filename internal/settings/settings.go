// Package settings — persistent runtime bot settings.
// Stored in a JSON file; if the file is absent, a new one is created, filled
// with defaults from defaults(). Thread-safe access — via Store.
//
// Extension principle: adding a new setting = a new field in Settings +
// default in defaults() + if needed a narrow method on Store. All callers
// depend on narrow interfaces (ISP), not on Store directly.
package settings

// Settings — snapshot of all runtime bot settings.
type Settings struct {
	// AllowedUsers — list of telegram user_ids allowed to talk to the bot.
	// If empty — the first one to write is added automatically (see Store.AllowFirstIfEmpty).
	AllowedUsers []int64 `json:"allowed_users"`
	// WorkDirs — per-user working directory (telegram user_id → absolute path).
	WorkDirs map[int64]string `json:"work_dirs"`
	// WhisperBin — path to the whisper-cli binary (or another compatible CLI).
	// On first start main does autodetect via exec.LookPath, the user
	// can override via /settings.
	WhisperBin string `json:"whisper_bin"`
	// WhisperModel — path to the ggml-*.bin model for transcription.
	// On first start main searches for ggml-base.bin in typical locations.
	WhisperModel string `json:"whisper_model"`
}

// defaults returns Settings filled with safe defaults.
// Used on the first launch and to normalize after load.
func defaults() Settings {
	return Settings{
		AllowedUsers: []int64{},
		WorkDirs:     map[int64]string{},
	}
}
