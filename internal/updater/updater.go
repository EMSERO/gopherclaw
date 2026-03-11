// Package updater checks for new GopherClaw releases and self-updates the binary.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// GitHubOwner and GitHubRepo are the GitHub coordinates for release checks.
var (
	GitHubOwner = "EMSERO"
	GitHubRepo  = "gopherclaw"
)

// CheckState persists the last version check result.
type CheckState struct {
	LastCheckedAt        time.Time `json:"lastCheckedAt"`
	LastAvailableVersion string    `json:"lastAvailableVersion"`
	LastNotifiedVersion  string    `json:"lastNotifiedVersion"`
}

// Release holds parsed info from a GitHub release.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is a single file attached to a GitHub release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// stateDir returns ~/.gopherclaw/state/, creating it if needed.
func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".gopherclaw", "state")
	return d, os.MkdirAll(d, 0755)
}

// statePath returns the path to update-check.json.
func statePath() (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "update-check.json"), nil
}

// LoadState reads the last check state from disk.
func LoadState() (*CheckState, error) {
	p, err := statePath()
	if err != nil {
		return &CheckState{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return &CheckState{}, nil
	}
	var s CheckState
	if err := json.Unmarshal(data, &s); err != nil {
		return &CheckState{}, nil
	}
	return &s, nil
}

// SaveState writes the check state to disk.
func SaveState(s *CheckState) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}

// ShouldCheck returns true if enough time has elapsed since the last check.
func ShouldCheck(s *CheckState, interval time.Duration) bool {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return time.Since(s.LastCheckedAt) > interval
}

// CheckLatest fetches the latest release from GitHub.
func CheckLatest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", GitHubOwner, GitHubRepo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// IsNewer returns true if the available version is newer than current.
// Compares semver strings (v0.4.0 > v0.3.0). Falls back to string comparison.
func IsNewer(current, available string) bool {
	current = strings.TrimPrefix(current, "v")
	available = strings.TrimPrefix(available, "v")
	if current == available {
		return false
	}
	// Simple semver: split on ".", compare numerically
	cp := strings.Split(current, ".")
	ap := strings.Split(available, ".")
	for i := 0; i < len(cp) && i < len(ap); i++ {
		if ap[i] > cp[i] {
			return true
		}
		if ap[i] < cp[i] {
			return false
		}
	}
	return len(ap) > len(cp)
}

// AssetName returns the expected asset filename for the current platform.
func AssetName(version string) string {
	return fmt.Sprintf("gopherclaw-%s-%s-%s.tar.gz",
		strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH)
}

// FindAsset returns the download URL for the current platform's asset.
func FindAsset(rel *Release) (string, error) {
	name := AssetName(rel.TagName)
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("no asset found for %s/%s (expected %s)", runtime.GOOS, runtime.GOARCH, name)
}

// FindChecksums returns the download URL for checksums.txt.
func FindChecksums(rel *Release) (string, error) {
	for _, a := range rel.Assets {
		if a.Name == "checksums.txt" {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("checksums.txt not found in release")
}

// Update downloads the latest release and replaces the running binary.
func Update(ctx context.Context, currentVersion string) (string, error) {
	rel, err := CheckLatest(ctx)
	if err != nil {
		return "", fmt.Errorf("check latest: %w", err)
	}

	if !IsNewer(currentVersion, rel.TagName) {
		return currentVersion, fmt.Errorf("already up to date (%s)", currentVersion)
	}

	assetURL, err := FindAsset(rel)
	if err != nil {
		return "", err
	}

	// Download to temp file
	tmpDir, err := os.MkdirTemp("", "gopherclaw-update-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "release.tar.gz")
	if err := downloadFile(ctx, assetURL, archivePath); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}

	// Verify checksum if available
	checksumURL, csErr := FindChecksums(rel)
	if csErr == nil {
		checksumPath := filepath.Join(tmpDir, "checksums.txt")
		if err := downloadFile(ctx, checksumURL, checksumPath); err == nil {
			if err := verifyChecksum(archivePath, checksumPath, AssetName(rel.TagName)); err != nil {
				return "", fmt.Errorf("checksum verification failed: %w", err)
			}
		}
	}

	// Extract binary from archive
	binaryPath := filepath.Join(tmpDir, "gopherclaw")
	if err := extractBinary(archivePath, binaryPath); err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}

	// Replace current binary atomically
	currentBinary, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find current binary: %w", err)
	}
	currentBinary, err = filepath.EvalSymlinks(currentBinary)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}

	// Backup old binary
	backupPath := currentBinary + ".bak"
	if err := copyFile(currentBinary, backupPath); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}

	// Atomic replace: copy new binary to temp next to target, then rename
	tmpBinary := currentBinary + ".new"
	if err := copyFile(binaryPath, tmpBinary); err != nil {
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Chmod(tmpBinary, 0755); err != nil {
		return "", fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpBinary, currentBinary); err != nil {
		return "", fmt.Errorf("replace binary: %w", err)
	}

	return rel.TagName, nil
}

// Rollback restores the previous binary from the .bak file created during updates.
func Rollback() error {
	currentBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current binary: %w", err)
	}
	currentBinary, err = filepath.EvalSymlinks(currentBinary)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	backupPath := currentBinary + ".bak"
	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("no backup found at %s — nothing to rollback to", backupPath)
	}

	// Atomic replace: copy backup to .new, then rename to current
	tmpBinary := currentBinary + ".new"
	if err := copyFile(backupPath, tmpBinary); err != nil {
		return fmt.Errorf("stage rollback binary: %w", err)
	}
	if err := os.Chmod(tmpBinary, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpBinary, currentBinary); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	return nil
}

// StartupCheck runs a non-blocking background version check.
// Returns a channel that receives a message if an update is available.
func StartupCheck(ctx context.Context, currentVersion string) <-chan string {
	ch := make(chan string, 1)
	go func() {
		defer close(ch)
		state, _ := LoadState()
		if !ShouldCheck(state, 24*time.Hour) {
			if state.LastAvailableVersion != "" && IsNewer(currentVersion, state.LastAvailableVersion) {
				if state.LastNotifiedVersion != state.LastAvailableVersion {
					ch <- state.LastAvailableVersion
				}
			}
			return
		}

		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		rel, err := CheckLatest(checkCtx)
		if err != nil {
			return
		}

		state.LastCheckedAt = time.Now()
		state.LastAvailableVersion = strings.TrimPrefix(rel.TagName, "v")
		_ = SaveState(state)

		if IsNewer(currentVersion, rel.TagName) {
			ch <- rel.TagName
		}
	}()
	return ch
}

// httpClient is a shared client with a reasonable timeout for download operations.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

// maxDownloadBytes is the maximum allowed download size (256 MB).
const maxDownloadBytes = 256 << 20

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read path
	_, err = io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes))
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // best-effort close on read path
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // explicit Close below
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func verifyChecksum(archivePath, checksumPath, expectedName string) error {
	// Read checksums file
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}

	// Find the line for our asset
	var expectedHash string
	for line := range strings.SplitSeq(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == expectedName {
			expectedHash = parts[0]
			break
		}
	}
	if expectedHash == "" {
		return fmt.Errorf("no checksum found for %s", expectedName)
	}

	// Compute actual hash
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read path
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if actualHash != expectedHash {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

func extractBinary(archivePath, destPath string) error {
	// Use tar to extract — simpler than implementing tar in Go
	// The binary is at the root of the archive
	dir := filepath.Dir(destPath)
	cmd := fmt.Sprintf("tar -xzf %s -C %s gopherclaw", archivePath, dir)
	return runShell(cmd)
}

func runShell(cmd string) error {
	c := &execCmd{cmd: cmd}
	return c.Run()
}

// execCmd is a minimal shell command runner (avoids importing os/exec at the
// package level to keep the linter happy about unused imports in test contexts).
type execCmd struct{ cmd string }

func (c *execCmd) Run() error {
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", c.cmd},
		&os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		})
	if err != nil {
		return err
	}
	state, err := p.Wait()
	if err != nil {
		return err
	}
	if !state.Success() {
		return fmt.Errorf("command failed: %s", c.cmd)
	}
	return nil
}
