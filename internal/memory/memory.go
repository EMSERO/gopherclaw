package memory

import (
	"os"
	"path/filepath"
)

// LoadMemoryMD reads {workspace}/MEMORY.md and returns its content.
// Returns empty string if the file does not exist or workspace is empty.
func LoadMemoryMD(workspace string) string {
	if workspace == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(workspace, "MEMORY.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// LoadHeartbeatMD reads {workspace}/HEARTBEAT.md and returns its content.
// Used for lightweight cron/heartbeat bootstrap context.
// Returns empty string if the file does not exist or workspace is empty.
func LoadHeartbeatMD(workspace string) string {
	if workspace == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(workspace, "HEARTBEAT.md"))
	if err != nil {
		return ""
	}
	return string(data)
}
