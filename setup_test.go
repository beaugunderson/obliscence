package main

import (
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
