package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// skillOmits names search flags the skill deliberately leaves out. Everything
// else must appear in it: the skill is how Claude learns the flags exist, and a
// flag it never mentions is a flag that never gets used.
var skillOmits = map[string]string{
	"semantic-weight": "a tuning knob for hybrid ranking, not a retrieval choice an agent needs to make",
}

// TestSkillDocumentsSearchFlags fails when a search flag is added without
// deciding whether the skill should teach it.
func TestSkillDocumentsSearchFlags(t *testing.T) {
	fields := reflect.TypeOf(SearchCmd{})
	for i := range fields.NumField() {
		f := fields.Field(i)
		// `arg:""` carries an empty value, so Lookup is the only way to see it.
		if _, isArg := f.Tag.Lookup("arg"); !f.IsExported() || isArg {
			continue // positional query argument, or internal state
		}

		flag := f.Tag.Get("name")
		if flag == "" {
			flag = kebab(f.Name)
		}
		if reason, omitted := skillOmits[flag]; omitted {
			if strings.Contains(skillContent, "--"+flag) {
				t.Errorf(
					"--%s is in the skill but listed in skillOmits (%s); drop the exemption",
					flag, reason,
				)
			}
			continue
		}
		if !strings.Contains(skillContent, "--"+flag) {
			t.Errorf(
				"--%s is missing from the search-history skill; document it in skillContent "+
					"or add it to skillOmits with a reason",
				flag,
			)
		}
	}
}

func TestPiExtensionIndexesLifecycle(t *testing.T) {
	for _, want := range []string{
		`pi.on("session_start"`,
		`pi.on("session_shutdown"`,
		`spawn("obliscence", ["hook"]`,
		`ctx.sessionManager.getSessionFile()`,
	} {
		if !strings.Contains(piExtensionContent, want) {
			t.Errorf("pi extension missing %q", want)
		}
	}
}

// settingsWith writes a settings.json under a temporary HOME and returns its
// path.
func settingsWith(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// hookCommands lists every command an event's matcher groups run, in order.
func hookCommands(t *testing.T, path, event string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, group := range settings.Hooks[event] {
		for _, hook := range group.Hooks {
			commands = append(commands, hook.Command)
		}
	}
	return commands
}

const ownHooks = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "my-session-start"}]}],
    "PreCompact": [{"matcher": "auto", "hooks": [{"type": "command", "command": "my-pre-compact"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "my-stop"}]}]
  }
}`

// A person's own hooks on the events obliscence uses run beside it, not
// instead of it.
func TestInstallHooksKeepsAPersonsOwnHooks(t *testing.T) {
	path := settingsWith(t, ownHooks)

	(&SetupCmd{}).installHooks()

	for event, want := range map[string][]string{
		"SessionStart": {"my-session-start", "obliscence hook"},
		"SessionEnd":   {"obliscence hook"},
		"PreCompact":   {"my-pre-compact", "obliscence hook"},
		"Stop":         {"my-stop"},
	} {
		if got := hookCommands(t, path, event); !reflect.DeepEqual(got, want) {
			t.Errorf("%s hooks = %q, want %q", event, got, want)
		}
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"model": "opus"`) {
		t.Errorf("lost an unrelated key:\n%s", data)
	}
}

func TestInstallHooksTwiceAddsOneEntry(t *testing.T) {
	path := settingsWith(t, ownHooks)

	(&SetupCmd{}).installHooks()
	(&SetupCmd{}).installHooks()

	want := []string{"my-session-start", "obliscence hook"}
	if got := hookCommands(t, path, "SessionStart"); !reflect.DeepEqual(got, want) {
		t.Errorf("SessionStart hooks = %q, want %q", got, want)
	}
}

// Uninstalling takes out obliscence's entries and nothing else, including a
// person's hook sharing a matcher group with one.
func TestRemoveHooksKeepsAPersonsOwnHooks(t *testing.T) {
	path := settingsWith(t, `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [{"type": "command", "command": "my-session-start"}]},
      {"matcher": "", "hooks": [{"type": "command", "command": "obliscence hook", "async": true}]}
    ],
    "SessionEnd": [
      {"matcher": "", "hooks": [
        {"type": "command", "command": "obliscence hook", "async": true},
        {"type": "command", "command": "my-session-end"}
      ]}
    ],
    "PreCompact": [{"matcher": "", "hooks": [{"type": "command", "command": "obliscence hook"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "my-stop"}]}]
  }
}`)

	(&UninstallCmd{}).removeHooks()

	for event, want := range map[string][]string{
		"SessionStart": {"my-session-start"},
		"SessionEnd":   {"my-session-end"},
		"PreCompact":   nil,
		"Stop":         {"my-stop"},
	} {
		if got := hookCommands(t, path, event); !reflect.DeepEqual(got, want) {
			t.Errorf("%s hooks = %q, want %q", event, got, want)
		}
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "PreCompact") {
		t.Errorf("left an empty PreCompact event:\n%s", data)
	}
}

func TestRemoveHooksDropsTheHooksKeyWhenNothingIsLeft(t *testing.T) {
	path := settingsWith(t, `{"model": "opus"}`)
	(&SetupCmd{}).installHooks()

	(&UninstallCmd{}).removeHooks()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hooks") {
		t.Errorf("left an empty hooks key:\n%s", data)
	}
}

func TestInstallAndRemovePiExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	path := filepath.Join(dir, "extensions", "obliscence.ts")

	(&SetupCmd{}).installPiExtension()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != piExtensionContent {
		t.Error("installed pi extension differs from source template")
	}

	(&UninstallCmd{}).removePiExtension()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pi extension remains after uninstall: %v", err)
	}
}

// kebab converts a Go field name to kong's default flag spelling.
func kebab(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('-')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
