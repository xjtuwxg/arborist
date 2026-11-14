// Package providers finds AI command-line tools already installed and signed
// in on this machine, and runs a prompt through one of them.
//
// Arborist has no API keys of its own. It reuses whatever subscription the
// user already has (Claude Code, Codex, Gemini CLI, opencode) or a local
// model (Ollama). Detection only checks that credential files exist; it never
// reads them.
package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider is one usable (or installed but not signed in) AI tool.
type Provider struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // "subscription" or "local"
	Path    string   `json:"path"`
	Version string   `json:"version"`
	Ready   bool     `json:"ready"`
	Note    string   `json:"note,omitempty"`
	Models  []string `json:"models,omitempty"`  // suggestions; any model name the tool accepts may be used
	Default string   `json:"default,omitempty"` // the tool's own configured model, when known
}

type spec struct {
	id, name, bin, kind string
	authFiles           []string // relative to home; existence means signed in
	envKeys             []string
	keychain            bool // macOS keeps the login in the keychain
}

var specs = []spec{
	{id: "claude", name: "Claude Code", bin: "claude", kind: "subscription",
		authFiles: []string{".claude/.credentials.json"}, envKeys: []string{"ANTHROPIC_API_KEY"}, keychain: true},
	{id: "codex", name: "Codex", bin: "codex", kind: "subscription",
		authFiles: []string{".codex/auth.json"}, envKeys: []string{"OPENAI_API_KEY"}},
	{id: "gemini", name: "Gemini CLI", bin: "gemini", kind: "subscription",
		authFiles: []string{".gemini/oauth_creds.json"}, envKeys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	{id: "opencode", name: "opencode", bin: "opencode", kind: "subscription",
		authFiles: []string{".local/share/opencode/auth.json"}},
	{id: "ollama", name: "Ollama", bin: "ollama", kind: "local"},
}

// extraDirs lists places CLIs get installed that a service's PATH often
// lacks (version managers, per-user prefixes).
func extraDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".local/bin"), filepath.Join(home, ".opencode/bin"),
		filepath.Join(home, ".bun/bin"), filepath.Join(home, ".npm-global/bin"),
		filepath.Join(home, ".claude/local"),
		"/home/linuxbrew/.linuxbrew/bin", "/opt/homebrew/bin", "/usr/local/bin",
	}
	nvm, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin"))
	sort.Sort(sort.Reverse(sort.StringSlice(nvm))) // newest node first
	return append(nvm, dirs...)
}

func find(bin string) string {
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	for _, d := range extraDirs() {
		p := filepath.Join(d, bin)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// env returns the environment for running a tool: the tool's own directory
// goes first on PATH so that `#!/usr/bin/env node` shims resolve.
func env(bin string) []string {
	e := os.Environ()
	for i, kv := range e {
		if strings.HasPrefix(kv, "PATH=") {
			e[i] = "PATH=" + filepath.Dir(bin) + string(os.PathListSeparator) + kv[5:]
			return e
		}
	}
	return append(e, "PATH="+filepath.Dir(bin))
}

func output(ctx context.Context, bin string, args ...string) string {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env(bin)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// Detect reports every known tool that is installed. Ready is true when it
// also looks signed in (or, for Ollama, has at least one model).
func Detect(ctx context.Context) []Provider {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	home, _ := os.UserHomeDir()
	found := make([]*Provider, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		path := find(s.bin)
		if path == "" {
			continue
		}
		p := &Provider{ID: s.id, Name: s.name, Kind: s.kind, Path: path}
		found[i] = p
		wg.Add(1)
		go func(s spec) {
			defer wg.Done()
			if v := strings.SplitN(output(ctx, path, "--version"), "\n", 2)[0]; v != "" && !strings.HasPrefix(v, "Warning") {
				p.Version = v
			}
			switch s.id {
			case "claude":
				p.Models = []string{"opus", "sonnet", "haiku"}
			case "codex":
				// The model line of config.toml is a preference, not a credential.
				if b, err := os.ReadFile(filepath.Join(home, ".codex/config.toml")); err == nil {
					for _, line := range strings.Split(string(b), "\n") {
						if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "model" {
							p.Default = strings.Trim(strings.TrimSpace(v), `"'`)
							p.Models = []string{p.Default}
							break
						}
					}
				}
			case "opencode":
				for _, line := range strings.Split(output(ctx, path, "models"), "\n") {
					if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, " ") {
						p.Models = append(p.Models, line)
					}
				}
			}
			if s.id == "ollama" {
				for _, line := range strings.Split(output(ctx, path, "list"), "\n")[1:] {
					if f := strings.Fields(line); len(f) > 0 {
						p.Models = append(p.Models, f[0])
					}
				}
				p.Ready = len(p.Models) > 0
				if !p.Ready {
					p.Note = "no models pulled, or the server is not running"
				}
				return
			}
			for _, f := range s.authFiles {
				if _, err := os.Stat(filepath.Join(home, f)); err == nil {
					p.Ready = true
					return
				}
			}
			for _, k := range s.envKeys {
				if os.Getenv(k) != "" {
					p.Ready, p.Note = true, "using "+k
					return
				}
			}
			if s.keychain && runtime.GOOS == "darwin" {
				p.Ready, p.Note = true, "sign-in not verified (kept in the keychain)"
				return
			}
			p.Note = "installed, not signed in"
		}(s)
	}
	wg.Wait()
	var out []Provider
	for _, p := range found {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

// Run sends prompt to the provider and returns its text answer. The tool is
// started in an empty directory with its own tools switched off or read-only:
// the prompt contains file names and README text from the workspace, which is
// untrusted, so the model must not be able to act on it.
//
// model may be empty, meaning the tool's own default.
func Run(ctx context.Context, p Provider, model, prompt string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sandbox := filepath.Join(cache, "arborist", "sandbox")
	if err := os.MkdirAll(sandbox, 0o700); err != nil {
		return "", err
	}
	if strings.HasPrefix(model, "-") {
		return "", fmt.Errorf("invalid model name %q", model)
	}
	// with adds the model flag when a model was chosen.
	with := func(flag string) []string {
		if model == "" {
			return nil
		}
		return []string{flag, model}
	}
	var args []string
	stdin := prompt
	var lastMsg string
	switch p.ID {
	case "claude":
		args = []string{"-p", "--tools", "", "--strict-mcp-config", "--no-session-persistence", "--output-format", "text"}
		args = append(args, with("--model")...)
	case "codex":
		f, err := os.CreateTemp(sandbox, "codex-*.txt")
		if err != nil {
			return "", err
		}
		f.Close()
		lastMsg = f.Name()
		defer os.Remove(lastMsg)
		args = append([]string{"exec"}, with("-m")...)
		args = append(args, "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "-o", lastMsg, "-")
	case "gemini":
		args, stdin = append(with("-m"), "-p", prompt), ""
	case "opencode":
		args, stdin = append(append([]string{"run"}, with("-m")...), prompt), ""
	case "ollama":
		if len(p.Models) == 0 {
			return "", errors.New("ollama has no models")
		}
		if model == "" {
			model = p.Models[0]
		}
		args = []string{"run", model}
	default:
		return "", fmt.Errorf("unknown provider %q", p.ID)
	}
	cmd := exec.CommandContext(ctx, p.Path, args...)
	cmd.Dir = sandbox
	cmd.Env = env(p.Path)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return "", fmt.Errorf("%s failed: %v: %s", p.Name, err, msg)
	}
	if lastMsg != "" {
		b, err := os.ReadFile(lastMsg)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return stdout.String(), nil
}
