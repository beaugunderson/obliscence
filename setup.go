package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type SetupCmd struct {
	Force bool `help:"Re-download even if files exist." short:"f"`
}

type UninstallCmd struct{}

func (cmd *UninstallCmd) Run(rc *RunContext) error {
	cmd.removeHooks()
	cmd.removeSkill()
	cmd.removeModels()

	fmt.Fprintln(
		os.Stderr,
		"uninstall complete. database at ~/.obliscence/db.sqlite was preserved.",
	)
	return nil
}

// removeHooks removes obliscence hook entries from ~/.claude/settings.json.
func (cmd *UninstallCmd) removeHooks() {
	settingsPath := expandPath("~/.claude/settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no settings.json found, skipping hook removal")
		return
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not parse settings.json, skipping hook removal")
		return
	}

	raw, ok := settings["hooks"]
	if !ok {
		fmt.Fprintln(os.Stderr, "no hooks found in settings.json")
		return
	}

	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &hooks); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not parse hooks, skipping hook removal")
		return
	}

	changed := false
	for _, event := range []string{"SessionStart", "SessionEnd", "PreCompact"} {
		existing, ok := hooks[event]
		if !ok {
			continue
		}
		if strings.Contains(string(existing), "obliscence hook") {
			delete(hooks, event)
			changed = true
		}
	}

	if !changed {
		fmt.Fprintln(os.Stderr, "no obliscence hooks found")
		return
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		hooksJSON, _ := json.Marshal(hooks)
		settings["hooks"] = hooksJSON
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not serialize settings: %v\n", err)
		return
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write settings.json: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "removed hooks from ~/.claude/settings.json")
}

// removeSkill removes the search-history skill from ~/.claude/skills/.
func (cmd *UninstallCmd) removeSkill() {
	skillDir := expandPath("~/.claude/skills/search-history")
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "skill not installed, skipping")
		return
	}

	if err := os.RemoveAll(skillDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove skill: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "removed skill from ~/.claude/skills/search-history/")
}

// removeModels removes the downloaded ONNX Runtime and model files.
func (cmd *UninstallCmd) removeModels() {
	dir := modelsDir()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "no models directory found, skipping")
		return
	}

	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove models: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "removed models from ~/.obliscence/models/")
}

const (
	onnxRuntimeVersion = "1.22.0"
	modelName          = "sentence-transformers/all-MiniLM-L6-v2"
	embeddingDim       = 384
)

func modelsDir() string {
	return filepath.Join(expandPath("~/.obliscence"), "models")
}

func onnxRuntimeLibPath() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(
			modelsDir(),
			"onnxruntime",
			fmt.Sprintf("libonnxruntime.%s.dylib", onnxRuntimeVersion),
		)
	}
	return filepath.Join(
		modelsDir(),
		"onnxruntime",
		fmt.Sprintf("libonnxruntime.so.%s", onnxRuntimeVersion),
	)
}

func onnxModelPath() string {
	return filepath.Join(modelsDir(), "all-MiniLM-L6-v2", "model.onnx")
}

func (cmd *SetupCmd) Run(rc *RunContext) error {
	dir := modelsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	if err := cmd.downloadONNXRuntime(); err != nil {
		return fmt.Errorf("downloading ONNX Runtime: %w", err)
	}

	if err := cmd.downloadModel(); err != nil {
		return fmt.Errorf("downloading model: %w", err)
	}

	cmd.installHooks()
	cmd.installSkill()

	fmt.Fprintln(os.Stderr, "setup complete. run 'obliscence index' to generate embeddings.")
	return nil
}

// installHooks adds SessionStart, SessionEnd, and PreCompact hooks to ~/.claude/settings.json.
func (cmd *SetupCmd) installHooks() {
	settingsPath := expandPath("~/.claude/settings.json")

	// Read existing settings.
	var settings map[string]json.RawMessage
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		settings = make(map[string]json.RawMessage)
	} else if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not parse settings.json, skipping hook install")
		return
	}

	// Check if our hooks are already present.
	var hooks map[string]json.RawMessage
	if raw, ok := settings["hooks"]; ok {
		json.Unmarshal(raw, &hooks)
	}
	if hooks == nil {
		hooks = make(map[string]json.RawMessage)
	}

	hookEntry := json.RawMessage(
		`[{"matcher":"","hooks":[{"type":"command","command":"obliscence hook","async":true,"suppressOutput":true}]}]`,
	)

	changed := false
	for _, event := range []string{"SessionStart", "SessionEnd", "PreCompact"} {
		existing, _ := hooks[event]
		if existing != nil && strings.Contains(string(existing), "obliscence hook") {
			continue
		}
		hooks[event] = hookEntry
		changed = true
	}

	if !changed {
		fmt.Fprintln(os.Stderr, "hooks already installed")
		return
	}

	hooksJSON, _ := json.Marshal(hooks)
	settings["hooks"] = hooksJSON

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not serialize settings: %v\n", err)
		return
	}

	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write settings.json: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "installed hooks in ~/.claude/settings.json")
}

const skillContent = `---
name: search-history
description: >-
  Search past conversations via obliscence. USE PROACTIVELY — invoke BEFORE codebase
  searches when the user recalls past work. Triggers: "remind me", "where does X live",
  "what did we do", "remember when", "how did we solve", "last time", "did we ever",
  "which repo", "which project", "in a previous session".
---

Search past Claude Code conversations using obliscence. Use ` + "`--json`" + ` for structured output,
` + "`--semantic`" + ` for meaning-based search, ` + "`--hybrid`" + ` for best results combining keyword + semantic.

## Choosing a search mode

- **Keyword search** (default): Best when the user remembers specific terms, error messages, file names, or tool names.
- **Semantic search** (` + "`--semantic`" + `): Best for conceptual queries like "how did we handle rate limiting". Finds results by meaning, not exact words.
- **Hybrid search** (` + "`--hybrid`" + `): Best general-purpose choice when unsure. Combines keyword and semantic via reciprocal rank fusion.

## Commands

` + "```" + `bash
# Search (start with --hybrid for best results)
obliscence search "$QUERY" --hybrid --json

# Keyword-only search (when user mentions specific terms)
obliscence search "$QUERY" --json

# Semantic search (when query is conceptual/natural language)
obliscence search "$QUERY" --semantic --json

# Filter by project
obliscence search "$QUERY" --hybrid --project PROJECT_NAME --json

# Filter by role (user messages vs assistant responses)
obliscence search "$QUERY" --hybrid --role user --json

# Filter by date range
obliscence search "$QUERY" --hybrid --after 2026-01-01 --before 2026-04-01 --json

# List recent sessions for a project
obliscence sessions --project PROJECT_NAME --json --limit 20

# Show a full conversation
obliscence show SESSION_ID_OR_SLUG

# Resume a past session
obliscence resume SESSION_ID_OR_SLUG

# List all projects
obliscence projects --json
` + "```" + `

## Tips

- Start broad, then narrow with ` + "`--project`" + ` or ` + "`--role`" + ` filters.
- Use ` + "`obliscence show SLUG`" + ` to read the full conversation once you find a relevant result.
- Session slugs (like ` + "`warm-wondering-quill`" + `) are stable identifiers.
- When the user says "we worked on X recently", add ` + "`--after`" + ` with a date ~2 weeks back.
`

// installSkill writes the search-history skill to ~/.claude/skills/.
func (cmd *SetupCmd) installSkill() {
	skillDir := expandPath("~/.claude/skills/search-history")
	skillPath := filepath.Join(skillDir, "SKILL.md")

	if !cmd.Force {
		if _, err := os.Stat(skillPath); err == nil {
			fmt.Fprintln(os.Stderr, "skill already installed")
			return
		}
	}

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create skill dir: %v\n", err)
		return
	}

	if err := os.WriteFile(skillPath, []byte(skillContent), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write skill: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "installed skill at ~/.claude/skills/search-history/")
}

func (cmd *SetupCmd) downloadONNXRuntime() error {
	dest := onnxRuntimeLibPath()
	if !cmd.Force {
		if _, err := os.Stat(dest); err == nil {
			fmt.Fprintln(
				os.Stderr,
				"ONNX Runtime already downloaded, skipping (use --force to re-download)",
			)
			return nil
		}
	}

	url := onnxRuntimeURL()
	if url == "" {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	fmt.Fprintf(os.Stderr, "downloading ONNX Runtime %s...\n", onnxRuntimeVersion)

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	return downloadAndExtractTgz(url, dest)
}

func (cmd *SetupCmd) downloadModel() error {
	dest := onnxModelPath()
	if !cmd.Force {
		if _, err := os.Stat(dest); err == nil {
			fmt.Fprintln(
				os.Stderr,
				"model already downloaded, skipping (use --force to re-download)",
			)
			return nil
		}
	}

	// Use quantized ARM64 model when available — ~4x smaller, ~2x faster inference.
	modelFile := "model.onnx"
	if runtime.GOARCH == "arm64" {
		modelFile = "model_qint8_arm64.onnx"
	}
	url := "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/onnx/" + modelFile
	fmt.Fprintf(os.Stderr, "downloading all-MiniLM-L6-v2 ONNX model (%s)...\n", modelFile)

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	return downloadFile(url, dest)
}

func onnxRuntimeURL() string {
	base := fmt.Sprintf(
		"https://github.com/microsoft/onnxruntime/releases/download/v%s",
		onnxRuntimeVersion,
	)
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return fmt.Sprintf("%s/onnxruntime-osx-arm64-%s.tgz", base, onnxRuntimeVersion)
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return fmt.Sprintf("%s/onnxruntime-osx-x86_64-%s.tgz", base, onnxRuntimeVersion)
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return fmt.Sprintf("%s/onnxruntime-linux-x64-%s.tgz", base, onnxRuntimeVersion)
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return fmt.Sprintf("%s/onnxruntime-linux-aarch64-%s.tgz", base, onnxRuntimeVersion)
	default:
		return ""
	}
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}

	fmt.Fprintf(os.Stderr, "  downloaded %s (%s)\n", filepath.Base(dest), formatBytes(written))
	return os.Rename(tmp, dest)
}

// downloadAndExtractTgz downloads a .tgz and extracts the shared library from it.
func downloadAndExtractTgz(url, destLib string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// Look for the versioned shared library in the archive (not the symlink).
	var libPattern string
	if runtime.GOOS == "darwin" {
		libPattern = fmt.Sprintf("libonnxruntime.%s.dylib", onnxRuntimeVersion)
	} else {
		libPattern = fmt.Sprintf("libonnxruntime.so.%s", onnxRuntimeVersion)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if strings.HasSuffix(hdr.Name, libPattern) && hdr.Typeflag == tar.TypeReg {
			tmp := destLib + ".tmp"
			f, err := os.Create(tmp)
			if err != nil {
				return err
			}
			written, err := io.Copy(f, tr)
			f.Close()
			if err != nil {
				os.Remove(tmp)
				return err
			}
			fmt.Fprintf(
				os.Stderr,
				"  extracted %s (%s)\n",
				filepath.Base(destLib),
				formatBytes(written),
			)
			return os.Rename(tmp, destLib)
		}
	}

	return fmt.Errorf("shared library not found in archive")
}
