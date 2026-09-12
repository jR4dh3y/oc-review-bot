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
	sandboxOpenCodeRoot = "/opt/opencode-runtime"
	sandboxPiRoot       = "/opt/pi-runtime"
	sandboxPiConfig     = "/pi-config"
	sandboxWorkspace    = "/workspace"
	sandboxDiffPath     = sandboxWorkspace + "/review-diff.patch"
	configFD            = 3
	authFD              = 4
	modelsFD            = 5

	sandboxHomeTmpfsSize      = "16777216" // 16 MiB
	sandboxConfigTmpfsSize    = "8388608"  // 8 MiB
	sandboxCacheTmpfsSize     = "33554432" // 32 MiB
	sandboxStateTmpfsSize     = "16777216" // 16 MiB
	sandboxRuntimeTmpfsSize   = "8388608"  // 8 MiB
	sandboxTemporaryTmpfsSize = "67108864" // 64 MiB
	sandboxVarTmpfsSize       = "16777216" // 16 MiB
)

type sandboxRuntime struct {
	engine     string
	bwrap      string
	runtimeDir string
	binary     string
}

// sandboxFiles are passed as read-only file descriptors, rather than mounting
// their host directory, so the sandbox cannot discover adjacent host files.
// models is optional: pi needs it only when the run's model names a custom
// provider that must be declared in PI_CODING_AGENT_DIR/models.json.
type sandboxFiles struct {
	config *os.File
	auth   *os.File
	models *os.File
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
	if f.models != nil {
		_ = f.models.Close()
	}
}

// extraFiles returns the descriptors in FD order: config is 3, auth is 4,
// and models is 5 when present.
func (f *sandboxFiles) extraFiles() []*os.File {
	files := []*os.File{f.config, f.auth}
	if f.models != nil {
		files = append(files, f.models)
	}
	return files
}

// Preflight verifies that the configured runtime can be launched inside the
// same Bubblewrap profile used for reviews. It performs no model request.
func Preflight(bin, runtimeDir, bubblewrapBin, engine string) error {
	if engine == "" {
		engine = EngineOpenCode2
	}
	if bin == "" {
		bin = "opencode2"
		if engine == EnginePi {
			bin = "pi"
		}
	}
	paths, err := resolveSandboxRuntime(Options{
		Engine:        engine,
		Bin:           bin,
		RuntimeDir:    runtimeDir,
		BubblewrapBin: bubblewrapBin,
	})
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "samik-bot-sandbox-check-*")
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
	// The pi catalog probe proves the staged agent resolves the OpenCode Zen
	// provider and the isolated credential store without a model request.
	probes := [][]string{{"--version"}, {"run", "--standalone", "--help"}}
	if engine == EnginePi {
		probes = [][]string{{"--version"}, {"--list-models", "opencode"}}
	}
	for _, probe := range probes {
		files, err := newSandboxFiles(tmp, "preflight", engine, "")
		if err != nil {
			return fmt.Errorf("%w: prepare capability probe", ErrSandboxUnavailable)
		}
		cmd := exec.CommandContext(ctx, paths.bwrap, sandboxCommand(paths, checkout, files, dataDir, probe)...)
		cmd.Dir = tmp
		cmd.Env = sandboxEnvironment(engine)
		cmd.ExtraFiles = files.extraFiles()
		runErr := cmd.Run()
		files.Close()
		if runErr != nil {
			if engine == EnginePi {
				return fmt.Errorf("%w: pi could not start inside Bubblewrap", ErrSandboxUnavailable)
			}
			return fmt.Errorf("%w: OpenCode 2 could not start inside Bubblewrap", ErrSandboxUnavailable)
		}
	}
	return nil
}

func resolveSandboxRuntime(o Options) (sandboxRuntime, error) {
	if runtime.GOOS != "linux" {
		return sandboxRuntime{}, fmt.Errorf("%w: Bubblewrap is required on Linux", ErrSandboxUnavailable)
	}
	engine := o.Engine
	if engine == "" {
		engine = EngineOpenCode2
	}
	root, binEnv, dirEnv := sandboxOpenCodeRoot, "OPENCODE_BIN", "OPENCODE_RUNTIME_DIR"
	if engine == EnginePi {
		root, binEnv, dirEnv = sandboxPiRoot, "PI_BIN", "PI_RUNTIME_DIR"
	}
	if !isEngineBinName(engine, o.Bin) {
		return sandboxRuntime{}, fmt.Errorf("%w: %s must name the %s executable", ErrSandboxUnavailable, binEnv, engine)
	}
	if strings.TrimSpace(o.RuntimeDir) == "" {
		return sandboxRuntime{}, fmt.Errorf("%w: %s is required", ErrSandboxUnavailable, dirEnv)
	}

	runtimeDir, err := canonicalDir(o.RuntimeDir)
	if err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: invalid %s", ErrSandboxUnavailable, dirEnv)
	}
	if runtimeDir == string(filepath.Separator) {
		return sandboxRuntime{}, fmt.Errorf("%w: %s must not be the filesystem root", ErrSandboxUnavailable, dirEnv)
	}
	binaryPath := o.Bin
	if !filepath.IsAbs(binaryPath) {
		binaryPath = filepath.Join(runtimeDir, "bin", binaryPath)
	}
	binary, err := canonicalExecutable(binaryPath)
	if err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: %s is not an executable", ErrSandboxUnavailable, binEnv)
	}
	if !isEngineBinName(engine, binary) {
		return sandboxRuntime{}, fmt.Errorf("%w: %s must resolve to the %s executable", ErrSandboxUnavailable, binEnv, engine)
	}
	if !pathWithin(runtimeDir, binary) {
		return sandboxRuntime{}, fmt.Errorf("%w: %s must be inside %s", ErrSandboxUnavailable, binEnv, dirEnv)
	}
	if err := validateTrustedRuntime(runtimeDir); err != nil {
		return sandboxRuntime{}, fmt.Errorf("%w: unsafe %s: %v", ErrSandboxUnavailable, dirEnv, err)
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
		return sandboxRuntime{}, fmt.Errorf("%w: %s escapes %s", ErrSandboxUnavailable, binEnv, dirEnv)
	}
	return sandboxRuntime{
		engine:     engine,
		bwrap:      bwrap,
		runtimeDir: runtimeDir,
		binary:     filepath.ToSlash(filepath.Join(root, rel)),
	}, nil
}

// isEngineBinName checks that the staged executable is named for the engine
// so one engine's runtime cannot be swapped in for another.
func isEngineBinName(engine, path string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe")
	if engine == EnginePi {
		return base == "pi"
	}
	return base == "opencode2"
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

// newSandboxFiles provisions the per-run engine configuration and credential
// stores. The pooled key is presented as the credential of the gateway the
// run's model names: the built-in "opencode" (Zen) provider, or the custom
// "orcarouter" provider declared alongside it.
func newSandboxFiles(tmp, apiKey, engine, model string) (*sandboxFiles, error) {
	secretsDir := filepath.Join(tmp, "sandbox-files")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return nil, err
	}
	provider := gatewayProviderForModel(model)
	if engine == EnginePi {
		// pi reads global settings and credentials from PI_CODING_AGENT_DIR:
		// an empty trust decision plus a disabled telemetry ping.
		config, err := jsonConfigFile(filepath.Join(secretsDir, "settings.json"), piSettings())
		if err != nil {
			return nil, err
		}
		files := &sandboxFiles{config: config}
		files.auth, err = jsonConfigFile(filepath.Join(secretsDir, "auth.json"), map[string]any{
			provider: map[string]string{"type": "api_key", "key": apiKey},
		})
		if err != nil {
			_ = config.Close()
			return nil, err
		}
		if provider == ProviderOrcaRouter {
			files.models, err = jsonConfigFile(filepath.Join(secretsDir, "models.json"), piOrcaRouterModels(apiKey, modelIDForProvider(model)))
			if err != nil {
				files.Close()
				return nil, err
			}
		}
		return files, nil
	}
	config, err := jsonConfigFile(filepath.Join(secretsDir, "opencode.json"), openCodeConfig(model))
	if err != nil {
		return nil, err
	}
	auth, err := jsonConfigFile(filepath.Join(secretsDir, "auth.json"), map[string]any{
		provider: map[string]string{"type": "api", "key": apiKey},
	})
	if err != nil {
		_ = config.Close()
		return nil, err
	}
	return &sandboxFiles{config: config, auth: auth}, nil
}

func piSettings() map[string]any {
	return map[string]any{
		"defaultProjectTrust":    "never",
		"enableInstallTelemetry": false,
	}
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

func sandboxCommand(paths sandboxRuntime, checkout string, files *sandboxFiles, dataDir string, agentArgs []string) []string {
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
		"--ro-bind", paths.runtimeDir, sandboxRoot(paths.engine),
	}
	// The reviewer executables are dynamically linked, but they do not need
	// broad host executable directories. Bind every canonical library
	// directory: glibc's compiled-in search path is /usr/lib on merged-usr
	// hosts (Arch), so mounting only /lib and /lib64 satisfies the kernel's
	// interpreter lookup but not the loader's dependency search.
	for _, path := range []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"} {
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
	)
	if paths.engine == EnginePi {
		// pi reads PI_CODING_AGENT_DIR: a writable tmpfs holding only the
		// per-run settings and credential files passed as read-only FDs. The
		// tmpfs must stay writable because pi caches its provider catalog.
		args = append(args,
			"--size", sandboxConfigTmpfsSize,
			"--tmpfs", sandboxPiConfig,
			"--ro-bind-data", fmt.Sprint(configFD), sandboxPiConfig+"/settings.json",
			"--ro-bind-data", fmt.Sprint(authFD), sandboxPiConfig+"/auth.json",
			// pi persists no sessions (--no-session) and needs no host data
			// directory, so /xdg-data stays an isolated tmpfs.
			"--size", sandboxStateTmpfsSize,
			"--tmpfs", "/xdg-data",
		)
		if files.models != nil {
			args = append(args,
				"--ro-bind-data", fmt.Sprint(modelsFD), sandboxPiConfig+"/models.json",
			)
		}
	} else {
		args = append(args,
			"--dir", "/xdg-config/opencode",
			"--ro-bind-data", fmt.Sprint(configFD), "/xdg-config/opencode/opencode.json",
			// The current beta cannot create its session database on a tmpfs
			// /xdg-data (Session.create fails); bind a per-run host directory
			// with the same lifetime instead. It carries no cross-run state.
			"--bind", dataDir, "/xdg-data",
			"--dir", "/xdg-data/opencode",
			"--ro-bind-data", fmt.Sprint(authFD), "/xdg-data/opencode/auth.json",
		)
	}
	args = append(args,
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
	for _, entry := range sandboxEnvironment(paths.engine) {
		key, value, _ := strings.Cut(entry, "=")
		args = append(args, "--setenv", key, value)
	}
	args = append(args, "--chdir", sandboxWorkspace, "--", paths.binary)
	return append(args, agentArgs...)
}

func sandboxRoot(engine string) string {
	if engine == EnginePi {
		return sandboxPiRoot
	}
	return sandboxOpenCodeRoot
}

func sandboxEnvironment(engine string) []string {
	env := []string{
		"PATH=" + sandboxRoot(engine) + "/bin:/usr/bin:/bin",
		"HOME=/home/reviewer",
		"XDG_CONFIG_HOME=/xdg-config",
		"XDG_DATA_HOME=/xdg-data",
		"XDG_CACHE_HOME=/xdg-cache",
		"XDG_STATE_HOME=/xdg-state",
		"XDG_RUNTIME_DIR=/run/user/0",
		"CI=1",
		"NO_COLOR=1",
		"TERM=dumb",
		"LC_ALL=C",
		"LANG=C",
		"TMPDIR=/tmp",
	}
	if engine == EnginePi {
		// The pooled key is never exposed through the child environment; it
		// arrives only through the isolated auth.json. Offline flags stop
		// update checks and telemetry so a review only talks to Zen.
		return append(env,
			"PI_CODING_AGENT_DIR="+sandboxPiConfig,
			"PI_OFFLINE=1",
			"PI_SKIP_VERSION_CHECK=1",
			"PI_TELEMETRY=0",
		)
	}
	return append(env,
		"OPENCODE_CONFIG=/xdg-config/opencode/opencode.json",
		"OPENCODE_CONFIG_DIR=/xdg-config/opencode",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_CONFIG_PROJECT_DISABLE=1",
	)
}
