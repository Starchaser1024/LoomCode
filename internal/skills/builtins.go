// Package skills implements LoomCode's three-tier SKILL.md catalog, on-demand SOP activation,
// directory-scoped tool registration, and remote skill installation.
package skills

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

//go:embed builtins/*
var builtinsFS embed.FS

// LoadBuiltins parses every embedded skill under builtins/. Each top-level
// subdir is a skill; we expect a SKILL.md inside. Bodies are loaded eagerly
// at startup since the FS is in-memory anyway. Errors on individual entries
// log nothing and are skipped — same policy as on-disk loaders.
//
// Note: embed.FS implements io/fs.FS and always uses forward-slash paths
// regardless of host OS, so path.Join (not filepath.Join) must be used.
func LoadBuiltins() []*Skill {
	var result []*Skill
	entries, err := builtinsFS.ReadDir("builtins")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		mdPath := path.Join("builtins", name, "SKILL.md")
		data, err := builtinsFS.ReadFile(mdPath)
		if err != nil {
			continue
		}
		skill, err := loadSkillFromBytes(name, data)
		if err != nil {
			continue
		}
		skill.IsDirectory = builtinHasToolJSON(name)
		result = append(result, skill)
	}
	return result
}

// builtinHasToolJSON peeks the embedded tree for a tool.json under the
// skill's dir. Used to flag directory-type builtins (e.g. review).
func builtinHasToolJSON(skillName string) bool {
	_, err := builtinsFS.ReadFile(path.Join("builtins", skillName, "tool.json"))
	return err == nil
}

// BuiltinToolJSON returns the raw tool.json bytes for an embedded skill, or
// nil if the skill isn't a directory-type builtin. Used by directory.go to
// hydrate the schema list.
func BuiltinToolJSON(skillName string) []byte {
	data, err := builtinsFS.ReadFile(path.Join("builtins", skillName, "tool.json"))
	if err != nil {
		return nil
	}
	return data
}

// walkBuiltinReferences returns all reference file paths under a builtin
// skill, used by tests and future loaders.
func walkBuiltinReferences(skillName string) []string {
	var out []string
	root := path.Join("builtins", skillName, "references")
	_ = fs.WalkDir(builtinsFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, ".md") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}
