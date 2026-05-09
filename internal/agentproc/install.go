package agentproc

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Pinned SDK version. Bump on Triage Factory release after verifying the
// new release in a spike — see scripts/clean-slate.sh notes. Keep the
// package.json template in sync with this constant.
const sdkVersion = "0.2.137"

// Embedded shim that translates the flag-based argv BuildArgs emits into
// Agent SDK Options. Materialized to disk at first install so the Node
// process can `import` from `node_modules/` next to it.
//
//go:embed wrapper.mjs
var wrapperJS []byte

var (
	installOnce sync.Once
	installPath string // populated on success
	installErr  error  // populated on first-run failure; sticky
)

// EnsureSDK installs the Agent SDK + wrapper into ~/.triagefactory/sdk/
// on first call and returns the absolute path to wrapper.mjs. Subsequent
// calls return the cached path. Concurrency-safe via sync.Once. A failure
// here sticks for the lifetime of the process so we don't retry npm
// install on every agent run when the user is missing Node.
//
// The install is idempotent: re-running against an already-populated dir
// only re-writes wrapper.mjs (cheap) and skips `npm install` when the
// pinned SDK version is already in node_modules.
func EnsureSDK() (string, error) {
	installOnce.Do(func() {
		installPath, installErr = doInstall()
	})
	return installPath, installErr
}

func doInstall() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	sdkDir := filepath.Join(home, ".triagefactory", "sdk")
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", sdkDir, err)
	}

	if err := writePackageJSON(sdkDir); err != nil {
		return "", err
	}

	wrapperPath := filepath.Join(sdkDir, "wrapper.mjs")
	if err := os.WriteFile(wrapperPath, wrapperJS, 0o644); err != nil {
		return "", fmt.Errorf("write wrapper.mjs: %w", err)
	}

	if err := checkNode(); err != nil {
		return "", err
	}

	if err := installSDKIfNeeded(sdkDir); err != nil {
		return "", err
	}

	return wrapperPath, nil
}

// writePackageJSON pins the SDK version. We re-write every install pass
// so a Triage Factory upgrade that bumps sdkVersion picks up the new
// pin even if the user already has an older copy installed (the
// installSDKIfNeeded check below will then trigger a re-install).
func writePackageJSON(sdkDir string) error {
	body := fmt.Sprintf(`{
  "name": "triagefactory-sdk-runtime",
  "private": true,
  "type": "module",
  "dependencies": {
    "@anthropic-ai/claude-agent-sdk": "%s"
  }
}
`, sdkVersion)
	return os.WriteFile(filepath.Join(sdkDir, "package.json"), []byte(body), 0o644)
}

// checkNode verifies a usable Node is on PATH. Required floor is 18 —
// the SDK itself documents Node 18+ as the minimum. We surface a
// human-readable error here so a missing-Node user sees "install Node
// 18+" rather than the opaque ENOENT exec.Command would otherwise raise
// when run.go later tries to spawn the wrapper.
func checkNode() error {
	out, err := exec.Command("node", "--version").Output()
	if err != nil {
		return fmt.Errorf("node not found on PATH: install Node 18+ (https://nodejs.org/) — required for the Triage Factory agent runtime")
	}
	major, err := parseNodeMajor(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("parse node --version output %q: %w", string(out), err)
	}
	if major < 18 {
		return fmt.Errorf("node %s is too old; Triage Factory requires Node 18+", strings.TrimSpace(string(out)))
	}
	return nil
}

func parseNodeMajor(version string) (int, error) {
	v := strings.TrimPrefix(version, "v")
	dot := strings.IndexByte(v, '.')
	if dot <= 0 {
		return 0, fmt.Errorf("unexpected format")
	}
	return strconv.Atoi(v[:dot])
}

// installSDKIfNeeded skips when the pinned SDK is already on disk.
// We check the installed package's version field rather than just
// directory existence so a sdkVersion bump in a future release
// re-triggers npm install.
func installSDKIfNeeded(sdkDir string) error {
	pkgFile := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json")
	body, err := os.ReadFile(pkgFile)
	if err == nil && strings.Contains(string(body), `"version": "`+sdkVersion+`"`) {
		return nil
	}

	cmd := exec.Command("npm", "install", "--no-audit", "--no-fund", "--silent")
	cmd.Dir = sdkDir
	cmd.Env = os.Environ()
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("npm install in %s failed: %w\n%s", sdkDir, err, string(combined))
	}
	return nil
}
