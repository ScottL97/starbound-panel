// Package config loads the panel configuration from environment variables.
//
// Every path is configurable so the same image can run on a bare host, in a
// container or in Kubernetes without rebuilding. Defaults assume a single
// writable data volume mounted at /data.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// Listen is the HTTP listen address, e.g. ":8080".
	Listen string

	// DataDir is the root of all persistent state.
	DataDir string
	// ServerDir holds the Starbound dedicated server installation.
	ServerDir string
	// StorageDir holds the universe and player saves (symlinked into ServerDir).
	StorageDir string
	// StateDir holds the panel state file.
	StateDir string
	// BackupDir receives the scheduled and manual backups.
	BackupDir string
	// ModsDir is the Starbound mods directory (inside ServerDir).
	ModsDir string
	// ModsDisabledDir keeps disabled mods outside the game's mod search path.
	ModsDisabledDir string

	// SteamCmd is the path to steamcmd.sh. It lives on the data volume so the
	// Steam Guard "sentry" files survive pod restarts: after one confirmed
	// login, later installs no longer ask for a code.
	SteamCmd string
	// SteamCmdDir is the working directory of steamcmd (workshop cache lives here).
	SteamCmdDir string
	// SteamCmdSeed is the copy shipped in the image, used to seed SteamCmdDir.
	SteamCmdSeed string
	// AppID is the Starbound Steam application id.
	AppID string
	// ServerBinary is the server executable relative to ServerDir.
	ServerBinary string
	// GamePort is only used for display and the connection hint in the UI.
	GamePort int
	// PublicAddress is what players type to connect, e.g. "example.com:21025".
	// When empty the UI falls back to the panel host name plus GamePort.
	PublicAddress string

	// AdminUser is the default administrator name for first-time setup.
	AdminUser string
	// AdminPassword, when set, seeds the administrator account on first start.
	AdminPassword string

	// APIToken, when set, enables "Authorization: Bearer <token>" access to the
	// JSON API for automation (for example an agent). It grants administrator
	// rights and skips the CSRF header check.
	APIToken string

	// LogBuffer is the number of console lines kept in memory.
	LogBuffer int

	// GitHubToken is optional and only lifts the GitHub API rate limit when
	// listing OpenStarbound releases.
	GitHubToken string

	// Version is the panel build version.
	Version string
}

// FromEnv builds a Config from SB_* environment variables.
func FromEnv(version string) Config {
	dataDir := env("SB_DATA_DIR", "/data")
	serverDir := env("SB_SERVER_DIR", filepath.Join(dataDir, "server"))
	steamCmd := env("SB_STEAMCMD", filepath.Join(dataDir, "steamcmd", "steamcmd.sh"))

	return Config{
		Listen:          env("SB_LISTEN", ":8080"),
		DataDir:         dataDir,
		ServerDir:       serverDir,
		StorageDir:      env("SB_STORAGE_DIR", filepath.Join(dataDir, "storage")),
		StateDir:        env("SB_STATE_DIR", filepath.Join(dataDir, "state")),
		BackupDir:       env("SB_BACKUP_DIR", filepath.Join(dataDir, "backups")),
		ModsDir:         filepath.Join(serverDir, "mods"),
		ModsDisabledDir: env("SB_MODS_DISABLED_DIR", filepath.Join(dataDir, "mods-disabled")),
		SteamCmd:        steamCmd,
		SteamCmdDir:     filepath.Dir(steamCmd),
		SteamCmdSeed:    env("SB_STEAMCMD_SEED", "/opt/steamcmd"),
		AppID:           env("SB_APP_ID", "211820"),
		ServerBinary:    env("SB_SERVER_BINARY", filepath.Join("linux", "starbound_server")),
		GamePort:        envInt("SB_GAME_PORT", 21025),
		PublicAddress:   os.Getenv("SB_PUBLIC_ADDRESS"),
		AdminUser:       env("SB_ADMIN_USER", "admin"),
		AdminPassword:   os.Getenv("SB_ADMIN_PASSWORD"),
		APIToken:        os.Getenv("SB_API_TOKEN"),
		GitHubToken:     os.Getenv("SB_GITHUB_TOKEN"),
		LogBuffer:       envInt("SB_LOG_BUFFER", 2000),
		Version:         version,
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
