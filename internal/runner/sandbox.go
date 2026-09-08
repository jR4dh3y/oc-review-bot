package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ErrSandboxUnavailable means a review cannot run in the mandatory Bubblewrap
// boundary. Callers must never fall back to direct OpenCode execution.
var ErrSandboxUnavailable = errors.New("bubblewrap review sandbox unavailable")

const (
	sandboxRuntimeRoot = "/opt/opencode-runtime"
	sandboxWorkspace   = "/workspace"
	sandboxDiffPath    = sandboxWorkspace + "/review-diff.patch"
	configFD           = 3
	authFD             = 4

	sandboxHomeTmpfsSize      = "16777216" // 16 MiB
	sandboxConfigTmpfsSize    = "8388608"  // 8 MiB
	sandboxCacheTmpfsSize     = "33554432" // 32 MiB
	sandboxStateTmpfsSize     = "16777216" // 16 MiB
	sandboxRuntimeTmpfsSize   = "8388608"  // 8 MiB
	sandboxTemporaryTmpfsSize = "67108864" // 64 MiB
	sandboxVarTmpfsSize       = "16777216" // 16 MiB
)

type sandboxRuntime struct {
	bwrap      string
	runtimeDir string
	binary     string
}

// sandboxFiles are passed as read-only file descriptors, rather than mounting
// their host directory, so the sandbox cannot discover adjacent host files.
type sandboxFiles struct {
	config *os.File
	auth   *os.File
}

func (f *sandboxFiles) Close() {
	if f == nil {
		return
	}
	if f.config != nil {
		_ = f.config.Close()
	}
	if f.auth != nil {
		_ = f.auth.Close()
	}
}

// Preflight verifies that the configured runtime can be launched inside the
// same Bubblewrap profile used for reviews. It performs no model request.
func Preflight(bin, runtimeDir, bubblewrapBin string) error {
	if bin == "" {
		bin = "opencode2"
	}
	paths, err := resolveSandboxRuntime(Options{
		Bin:           bin,
		RuntimeDir:    runtimeDir,
		BubblewrapBin: bubblewrapBin,
	})
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "oc-review-sandbox-check-*")
	if err != nil {
		return fmt.Errorf("%w: create capability probe", ErrSandboxUnavailable)
	}
	defer os.RemoveAll(tmp)
	checkout := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return fmt.Errorf("%w: prepare capability probe", ErrSandboxUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dataDir := filepath.Join(tmp, "xdg-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("%w: prepare capability probe", ErrSandboxUnavailable)
	}
	for _, opencodeArgs := range [][]string{{"--version"}, {"run", "--standalone", "--help"}} {
		files, err := newSandboxFiles(tmp, "preflight")
		if err != nil {
			return fmt.Errorf("%w: prepare capability probe", ErrSandboxUnavailable)
		}
		cmd := exec.CommandContext(ctx, paths.bwrap, sandboxCommand(paths, checkout, files, dataDir, opencodeArgs)...)
		cmd.Dir = tmp
		cmd.Env = sandboxEnvironment()
		cmd.ExtraFiles = []*os.File{files.config, files.auth}
		runErr := cmd.Run()
		files.Close()
		if runErr != nil {
			return fmt.Errorf("%w: OpenCode 2 could not start inside Bubblewrap", ErrSandboxUnavailable)
		}
	}
	return nil
}

func resolveSandboxRuntime(o Options) (sandboxRuntime, error) {
	if runtime.GOOS != "linux" {
		return sandboxRuntime{}, fmt.Errorf("%w: Bubblewrap is required on Linux", ErrSandboxUnavailable)
	}
	if !isOpenCode2Name(o.Bin) {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_BIN must name opencode2", ErrSandboxUnavailable)
	}
	if strings.TrimSpace(o.RuntimeDir) == "" {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_RUNTIME_DIR is required", ErrSandboxUnavailable)
	}

	runtimeDir, err := canonicalDir(o.RuntimeDir)
	if err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: invalid OPENCODE_RUNTIME_DIR", ErrSandboxUnavailable)
	}
	if runtimeDir == string(filepath.Separator) {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_RUNTIME_DIR must not be the filesystem root", ErrSandboxUnavailable)
	}
	binaryPath := o.Bin
	if !filepath.IsAbs(binaryPath) {
		binaryPath = filepath.Join(runtimeDir, "bin", binaryPath)
	}
	binary, err := canonicalExecutable(binaryPath)
	if err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_BIN is not an executable", ErrSandboxUnavailable)
	}
	if !isOpenCode2Name(binary) {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_BIN must resolve to opencode2", ErrSandboxUnavailable)
	}
	if !pathWithin(runtimeDir, binary) {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_BIN must be inside OPENCODE_RUNTIME_DIR", ErrSandboxUnavailable)
	}
	if err := validateTrustedRuntime(runtimeDir); err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: unsafe OPENCODE_RUNTIME_DIR: %v", ErrSandboxUnavailable, err)
	}

	if strings.TrimSpace(o.BubblewrapBin) == "" || !filepath.IsAbs(o.BubblewrapBin) {
		return sandboxRuntime{}, fmt.Errorf("%w: BUBBLEWRAP_BIN must be an absolute executable path", ErrSandboxUnavailable)
	}
	bwrap, err := canonicalExecutable(o.BubblewrapBin)
	if err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: bwrap executable was not found", ErrSandboxUnavailable)
	}
	if err := validateTrustedFile(bwrap); err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: unsafe bwrap executable: %v", ErrSandboxUnavailable, err)
	}
	if err := validateTrustedAncestors(bwrap); err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: unsafe bwrap executable path: %v", ErrSandboxUnavailable, err)
	}
	if _, err := os.Lstat("/usr"); err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: required system runtime is missing", ErrSandboxUnavailable)
	}

	rel, err := filepath.Rel(runtimeDir, binary)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return sandboxRuntime{}, fmt.Errorf("%w: OPENCODE_BIN escapes OPENCODE_RUNTIME_DIR", ErrSandboxUnavailable)
	}
	return sandboxRuntime{
		bwrap:      bwrap,
		runtimeDir: runtimeDir,
		binary:     filepath.ToSlash(filepath.Join(sandboxRuntimeRoot, rel)),
	}, nil
}

func isOpenCode2Name(path string) bool {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe") == "opencode2"
}

func canonicalDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path is not absolute")
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("directory must not be a symlink")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != path {
		return "", errors.New("directory path must not traverse symlinks")
	}
	info, err = os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("not a directory")
	}
	return filepath.Clean(resolved), nil
}

func canonicalExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("executable path is not absolute")
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("executable must not be a symlink")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != path {
		return "", errors.New("executable path must not traverse symlinks")
	}
	info, err = os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("not an executable file")
	}
	return filepath.Clean(resolved), nil
}

// validateTrustedRuntime rejects a mutable or symlinked runtime before it is
// bind-mounted. A writable runtime could replace the reviewer or expose host
// paths through a symlink after Bubblewrap starts.
func validateTrustedRuntime(root string) error {
	if err := validateTrustedAncestors(root); err != nil {
		return err
	}
	entries := 0
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > 10_000 {
			return errors.New("runtime has too many entries")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink at %s", path)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported file type at %s", path)
		}
		return validateTrustedFile(path)
	})
}

func validateTrustedFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("path is writable by group or others")
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return errors.New("path has set-ID mode bits")
	}
	if err := validateTrustedOwner(info); err != nil {
		return err
	}
	return nil
}

func validateTrustedAncestors(target string) error {
	for dir := filepath.Dir(target); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("ancestor %s is not a direct directory", dir)
		}
		if info.Mode().Perm()&0o022 != 0 && !isRootOwnedStickyDirectory(info) {
			return fmt.Errorf("ancestor %s is writable by group or others", dir)
		}
		if err := validateTrustedOwner(info); err != nil {
			return fmt.Errorf("ancestor %s: %w", dir, err)
		}
		if dir == string(filepath.Separator) {
			return nil
		}
	}
}

func isRootOwnedStickyDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
}

func validateTrustedOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine path owner")
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != uid {
		return fmt.Errorf("path owner uid %d is neither root nor the service user", stat.Uid)
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func newSandboxFiles(tmp, apiKey string) (*sandboxFiles, error) {
	secretsDir := filepath.Join(tmp, "sandbox-files")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return nil, err
	}
	config, err := jsonConfigFile(filepath.Join(secretsDir, "opencode.json"), openCodeConfig())
	if err != nil {
		return nil, err
	}
	auth, err := jsonConfigFile(filepath.Join(secretsDir, "auth.json"), map[string]any{
		"opencode": map[string]string{"type": "api", "key": apiKey},
	})
	if err != nil {
		_ = config.Close()
		return nil, err
	}
	return &sandboxFiles{config: config, auth: auth}, nil
}

func jsonConfigFile(path string, value any) (*os.File, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return os.Open(path)
}

func sandboxCommand(paths sandboxRuntime, checkout string, files *sandboxFiles, dataDir string, opencodeArgs []string) []string {
	args := []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		// A cgroup namespace hides host topology but does not apply resource
		// quotas; deployment-owned cgroup limits remain mandatory.
		"--unshare-cgroup-try",
		// Zen needs provider egress. Operators must restrict this with their
		// network policy because Bubblewrap cannot express hostname allowlists.
		"--share-net",
		"--cap-drop", "ALL",
		"--clearenv",
		"--ro-bind", paths.runtimeDir, sandboxRuntimeRoot,
	}
	// The OpenCode binary is dynamically linked, but it does not need broad
	// host executable directories. /lib and /lib64 provide only its loader and
	// shared libraries on supported Linux hosts.
	for _, path := range []string{"/lib", "/lib64"} {
		if _, err := os.Lstat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args,
		"--dir", "/etc",
		"--dir", "/etc/ssl",
		"--dir", "/etc/pki",
		"--dir", "/etc/pki/tls",
		"--dir", "/etc/pki/tls/certs",
		"--dir", "/etc/pki/ca-trust",
		"--dir", "/etc/pki/ca-trust/extracted",
		"--dir", "/etc/pki/ca-trust/extracted/pem",
	)
	for _, path := range []string{
		"/etc/resolv.conf",
		"/etc/hosts",
		"/etc/nsswitch.conf",
		"/etc/ssl/certs",
		"/etc/ssl/openssl.cnf",
		"/etc/ssl/cert.pem",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	} {
		if _, err := os.Lstat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args,
		"--size", sandboxHomeTmpfsSize,
		"--tmpfs", "/home",
		"--dir", "/home/reviewer",
		"--size", sandboxConfigTmpfsSize,
		"--tmpfs", "/xdg-config",
		"--dir", "/xdg-config/opencode",
		"--ro-bind-data", fmt.Sprint(configFD), "/xdg-config/opencode/opencode.json",
		// The current beta cannot create its session database on a tmpfs
		// /xdg-data (Session.create fails); bind a per-run host directory
		// with the same lifetime instead. It carries no cross-run state.
		"--bind", dataDir, "/xdg-data",
		"--dir", "/xdg-data/opencode",
		"--ro-bind-data", fmt.Sprint(authFD), "/xdg-data/opencode/auth.json",
		"--size", sandboxCacheTmpfsSize,
		"--tmpfs", "/xdg-cache",
		"--size", sandboxStateTmpfsSize,
		"--tmpfs", "/xdg-state",
		"--size", sandboxRuntimeTmpfsSize,
		"--tmpfs", "/run",
		"--dir", "/run/user",
		"--dir", "/run/user/0",
		"--size", sandboxTemporaryTmpfsSize,
		"--tmpfs", "/tmp",
		"--size", sandboxVarTmpfsSize,
		"--tmpfs", "/var",
		"--ro-bind", checkout, sandboxWorkspace,
		"--proc", "/proc",
		"--dev", "/dev",
	)
	for _, entry := range sandboxEnvironment() {
		key, value, _ := strings.Cut(entry, "=")
		args = append(args, "--setenv", key, value)
	}
	args = append(args, "--chdir", sandboxWorkspace, "--", paths.binary)
	return append(args, opencodeArgs...)
}

func sandboxEnvironment() []string {
	return []string{
		"PATH=" + sandboxRuntimeRoot + "/bin:/usr/bin:/bin",
		"HOME=/home/reviewer",
		"XDG_CONFIG_HOME=/xdg-config",
		"XDG_DATA_HOME=/xdg-data",
		"XDG_CACHE_HOME=/xdg-cache",
		"XDG_STATE_HOME=/xdg-state",
		"XDG_RUNTIME_DIR=/run/user/0",
		"OPENCODE_CONFIG=/xdg-config/opencode/opencode.json",
		"OPENCODE_CONFIG_DIR=/xdg-config/opencode",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_CONFIG_PROJECT_DISABLE=1",
		"CI=1",
		"NO_COLOR=1",
		"TERM=dumb",
		"LC_ALL=C",
		"LANG=C",
		"TMPDIR=/tmp",
	}
}
