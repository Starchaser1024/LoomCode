// Package permissions enforces path sandboxing, mode matrices, and pattern rules for tool execution.
package permissions

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Starchaser1024/LoomCode/internal/tools"
)

type DecisionEffect string

const (
	Allow DecisionEffect = "allow"
	Deny  DecisionEffect = "deny"
	Ask   DecisionEffect = "ask"
)

type Decision struct {
	Effect DecisionEffect
	Reason string
}

type PermissionMode string

const (
	ModeDefault     PermissionMode = "default"
	ModeAcceptEdits PermissionMode = "acceptEdits"
	ModePlan        PermissionMode = "plan"
	ModeBypass      PermissionMode = "bypassPermissions"
)

var modeMatrix = map[PermissionMode]map[tools.ToolCategory]DecisionEffect{
	ModeDefault:     {tools.CategoryRead: Allow, tools.CategoryWrite: Ask, tools.CategoryCommand: Ask},
	ModeAcceptEdits: {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Ask},
	ModeBypass:      {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Allow},
}

func ModeDecide(mode PermissionMode, category tools.ToolCategory) DecisionEffect {
	m, ok := modeMatrix[mode]
	if !ok {
		return Ask
	}
	return m[category]
}

// Layer 1: Dangerous command detection

type dangerousPattern struct {
	re     *regexp.Regexp
	reason string
}

var defaultDangerousPatterns = []dangerousPattern{
	{regexp.MustCompile(`\brm\s+(?:-[a-zA-Z]*[rf][a-zA-Z]*\s+|--(?:recursive|force|no-preserve-root)\s+){1,4}/(?:\*|\s*$|\s*[;&|])`), "recursive force delete root"},
	{regexp.MustCompile(`\brm\s+(?:-[a-zA-Z]+\s+|--(?:recursive|force)\s+)*["']?\.git["']?(?:/|\s*$|\s*[;&|])`), "delete .git repository metadata"},
	{regexp.MustCompile(`\bgit\s+(?:\S+\s+)*reset\s+(?:\S+\s+)*--hard(?:\s|$|[;&|])`), "destructive git reset --hard"},
	{regexp.MustCompile(`\bgit\s+(?:\S+\s+)*clean\s+(?:-[a-zA-Z]*f|--force)`), "destructive git clean"},
	{regexp.MustCompile(`\bgit\s+(?:\S+\s+)*checkout\s+(?:--\s+)?\.(?:\s*$|\s+[;&|]|\s*$)`), "destructive git checkout discarding workspace changes"},
	{regexp.MustCompile(`\bgit\s+(?:\S+\s+)*restore\s+(?:--worktree\s+)?(?:--\s+)?\.(?:\s*$|\s+[;&|])`), "destructive git restore discarding workspace changes"},
	{regexp.MustCompile(`mkfs\.`), "format disk"},
	{regexp.MustCompile(`dd\s+if=.*of=/dev/`), "direct write to disk device"},
	{regexp.MustCompile(`chmod\s+-R\s+777\s+/`), "recursive chmod root"},
	{regexp.MustCompile(`:\(\)\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`curl\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`wget\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`>\s*/dev/sd`), "overwrite disk device"},
}

func DetectDangerous(command string) (bool, string) {
	for _, p := range defaultDangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	return false, ""
}

// Layer 2: Path sandbox

type PathSandbox struct {
	projectRoot  string
	primaryRoots []string
	deniedRoots  []string
	tempRoot     string
}

func NewPathSandbox(projectRoot string, extraAllowed ...string) *PathSandbox {
	sb := newSandbox(projectRoot, true, extraAllowed...)
	return sb
}

// NewStrictPathSandbox confines file and command working-directory access strictly to
// workspaceRoot (plus any extraAllowed roots), without granting global os.TempDir() access.
func NewStrictPathSandbox(workspaceRoot string, extraAllowed ...string) *PathSandbox {
	return newSandbox(workspaceRoot, false, extraAllowed...)
}

func newSandbox(projectRoot string, allowTemp bool, extraAllowed ...string) *PathSandbox {
	root := canonicalizeSandboxPath(projectRoot)
	primary := []string{root}
	for _, p := range extraAllowed {
		if p == "" {
			continue
		}
		primary = append(primary, canonicalizeSandboxPath(p))
	}

	var denied []string
	if parentRepo := detectWorktreeParentRepo(root); parentRepo != "" && !sameCleanPath(parentRepo, root) {
		denied = append(denied, parentRepo)
	}

	tempRoot := ""
	if allowTemp {
		tempRoot = canonicalizeSandboxPath(os.TempDir())
	}

	return &PathSandbox{
		projectRoot:  root,
		primaryRoots: primary,
		deniedRoots:  denied,
		tempRoot:     tempRoot,
	}
}

// ProjectRoot returns the canonical active workspace or worktree root.
func (s *PathSandbox) ProjectRoot() string {
	if s == nil {
		return ""
	}
	return s.projectRoot
}

func (s *PathSandbox) Check(path string) (bool, string) {
	if s == nil {
		return true, ""
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false, "empty path"
	}

	candidate := trimmed
	if !filepath.IsAbs(candidate) && !strings.HasPrefix(candidate, "/") && !strings.HasPrefix(candidate, `\`) && s.projectRoot != "" {
		candidate = filepath.Join(s.projectRoot, candidate)
	}
	resolved := canonicalizeSandboxPath(candidate)

	for _, root := range s.primaryRoots {
		if root != "" && isWithinDir(resolved, root) {
			return true, ""
		}
	}

	// If running inside an isolated .loomcode/worktrees/<slug> workspace, explicitly reject
	// escaping into the parent checkout even when both live under os.TempDir() in tests.
	for _, denied := range s.deniedRoots {
		if denied != "" && isWithinDir(resolved, denied) {
			return false, fmt.Sprintf("path %s escapes worktree into parent checkout", path)
		}
	}

	if s.tempRoot != "" && isWithinDir(resolved, s.tempRoot) {
		return true, ""
	}

	return false, fmt.Sprintf("path %s outside sandbox", path)
}

var (
	shellCdPattern  = regexp.MustCompile(`(?:^|[;&|]\s*|\n\s*)cd\s+([^\s;&|]+)`)
	gitDashCPattern = regexp.MustCompile(`\bgit\s+-C\s+([^\s;&|]+)`)
)

// CheckCommandDir verifies that explicit command working-directory arguments (args["cwd"],
// `cd <dir>`, or `git -C <dir>`) do not escape the active workspace/worktree sandbox.
func (s *PathSandbox) CheckCommandDir(command string, args map[string]any) (bool, string) {
	if s == nil {
		return true, ""
	}
	if args != nil {
		if cwd, ok := args["cwd"].(string); ok && strings.TrimSpace(cwd) != "" {
			if ok, reason := s.Check(cwd); !ok {
				return false, fmt.Sprintf("command working directory blocked: %s", reason)
			}
		}
	}

	for _, re := range []*regexp.Regexp{shellCdPattern, gitDashCPattern} {
		matches := re.FindAllStringSubmatch(command, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			target := strings.Trim(m[1], `"'`)
			if target == "" || target == "-" || strings.HasPrefix(target, "$") || strings.HasPrefix(target, "~") {
				if strings.HasPrefix(target, "~") {
					return false, fmt.Sprintf("command directory %s escapes workspace sandbox", target)
				}
				continue
			}
			if ok, reason := s.Check(target); !ok {
				return false, fmt.Sprintf("command directory %s blocked: %s", target, reason)
			}
		}
	}
	return true, ""
}

// canonicalizeSandboxPath resolves symlinks on the deepest existing prefix of p and re-attaches
// any not-yet-created trailing path segments, preventing symlink escape on new-file creation.
func canonicalizeSandboxPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	abs = filepath.Clean(abs)

	cur := abs
	var missingTail []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(missingTail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missingTail[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		missingTail = append(missingTail, filepath.Base(cur))
		cur = parent
	}
	return abs
}

func isWithinDir(target, root string) bool {
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(filepath.VolumeName(target), filepath.VolumeName(root)) {
			return false
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

func sameCleanPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func detectWorktreeParentRepo(root string) string {
	slash := filepath.ToSlash(filepath.Clean(root))
	const marker = "/.loomcode/worktrees/"
	idx := strings.LastIndex(slash, marker)
	if idx <= 0 {
		return ""
	}
	return filepath.FromSlash(slash[:idx])
}

// Layer 3: Rule engine

type RuleEffect string

const (
	RuleAllow RuleEffect = "allow"
	RuleDeny  RuleEffect = "deny"
)

type Rule struct {
	ToolName string
	Pattern  string
	Effect   RuleEffect
}

func (r Rule) Matches(toolName, content string) bool {
	if r.ToolName != toolName {
		return false
	}
	matched, _ := filepath.Match(r.Pattern, content)
	return matched
}

type RuleEngine struct {
	UserPath    string
	ProjectPath string
	LocalPath   string
}

func (e *RuleEngine) Evaluate(toolName, content string) *RuleEffect {
	for _, path := range []string{e.UserPath, e.ProjectPath, e.LocalPath} {
		rules := loadRulesFile(path)
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].Matches(toolName, content) {
				eff := rules[i].Effect
				return &eff
			}
		}
	}
	return nil
}

func (e *RuleEngine) AppendLocalRule(r Rule) {
	if e.LocalPath == "" {
		return
	}
	os.MkdirAll(filepath.Dir(e.LocalPath), 0o755)
	rules := loadRulesFile(e.LocalPath)
	rules = append(rules, r)
	var entries []map[string]string
	for _, rule := range rules {
		entries = append(entries, map[string]string{
			"rule":   fmt.Sprintf("%s(%s)", rule.ToolName, rule.Pattern),
			"effect": string(rule.Effect),
		})
	}
	data, _ := yaml.Marshal(entries)
	os.WriteFile(e.LocalPath, data, 0o644)
}

func loadRulesFile(path string) []Rule {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []struct {
		RuleStr string `yaml:"rule"`
		Effect  string `yaml:"effect"`
	}
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var rules []Rule
	for _, e := range entries {
		if e.Effect != "allow" && e.Effect != "deny" {
			continue
		}
		r, err := parseRule(e.RuleStr, RuleEffect(e.Effect))
		if err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

var ruleRE = regexp.MustCompile(`^(\w+)\((.+)\)$`)

func parseRule(raw string, effect RuleEffect) (Rule, error) {
	m := ruleRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Rule{}, fmt.Errorf("invalid rule syntax: %s", raw)
	}
	return Rule{ToolName: m[1], Pattern: m[2], Effect: effect}, nil
}

// Safe read-only commands that don't need permission

var safeCommandPrefixes = []string{
	"ls", "dir", "pwd", "echo", "cat", "head", "tail", "wc",
	"find", "which", "whereis", "whoami", "hostname", "uname",
	"date", "cal", "uptime", "df", "du", "free", "env", "printenv",
	"file", "stat", "readlink", "realpath", "basename", "dirname",
	"sort", "uniq", "tr", "cut", "awk", "sed", "grep", "egrep", "fgrep",
	"diff", "comm", "tee", "xargs", "true", "false", "test",
	"git status", "git log", "git diff", "git show", "git branch",
	"git tag", "git remote", "git rev-parse", "git ls-files",
	"git blame", "git stash list", "go version", "go env",
	"node -v", "npm -v", "npx", "python --version", "pip list",
	"cargo --version", "rustc --version",
}

func IsSafeCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	for _, prefix := range safeCommandPrefixes {
		if cmd == prefix || strings.HasPrefix(cmd, prefix+" ") || strings.HasPrefix(cmd, prefix+"\t") {
			if !strings.Contains(cmd, ">") && !strings.Contains(cmd, "|") &&
				!strings.Contains(cmd, ";") && !strings.Contains(cmd, "&&") &&
				!strings.Contains(cmd, "$(") && !strings.Contains(cmd, "`") {
				return true
			}
		}
	}
	return false
}

// Content extraction for rule matching

var contentFields = map[string]string{
	"Bash": "command", "ReadFile": "file_path", "WriteFile": "file_path",
	"EditFile": "file_path", "Glob": "pattern", "Grep": "pattern",
}

func ExtractContent(toolName string, args map[string]any) string {
	field, ok := contentFields[toolName]
	if !ok {
		return ""
	}
	v, _ := args[field].(string)
	return v
}

// Layer 4+5: Permission Checker (orchestrates all layers)

type Checker struct {
	Sandbox      *PathSandbox
	RuleEngine   *RuleEngine
	Mode         PermissionMode
	PlanFilePath string
}

func NewChecker(sandbox *PathSandbox, ruleEngine *RuleEngine, mode PermissionMode) *Checker {
	return &Checker{Sandbox: sandbox, RuleEngine: ruleEngine, Mode: mode}
}

func (c *Checker) Check(tool tools.Tool, args map[string]any) Decision {
	content := ExtractContent(tool.Name(), args)
	cat := tool.Category()

	// Layer 0: Plan mode plan-file write exception
	if c.Mode == ModePlan && cat == tools.CategoryWrite && isPlanFile(content, c.PlanFilePath) {
		return Decision{Effect: Allow, Reason: "Plan mode: plan file write allowed"}
	}

	// Layer 1: dangerous command (Bash only) — evaluated before safe-prefix checks so nothing
	// destructive can ever bypass safety gates.
	if cat == tools.CategoryCommand {
		hit, reason := DetectDangerous(content)
		if hit {
			return Decision{Effect: Deny, Reason: fmt.Sprintf("Dangerous command blocked: %s", reason)}
		}
		if c.Sandbox != nil {
			if ok, reason := c.Sandbox.CheckCommandDir(content, args); !ok {
				return Decision{Effect: Deny, Reason: fmt.Sprintf("Path sandbox: %s", reason)}
			}
		}
	}

	// Layer 2: safe read-only commands (auto-allow)
	if cat == tools.CategoryCommand && IsSafeCommand(content) {
		return Decision{Effect: Allow, Reason: "Safe read-only command"}
	}

	// Layer 3: path sandbox (file tools)
	if (cat == tools.CategoryRead || cat == tools.CategoryWrite) && content != "" {
		ok, reason := c.Sandbox.Check(content)
		if !ok {
			return Decision{Effect: Deny, Reason: fmt.Sprintf("Path sandbox: %s", reason)}
		}
	}

	// Layer 4: rule engine
	if c.RuleEngine != nil {
		ruleResult := c.RuleEngine.Evaluate(tool.Name(), content)
		if ruleResult != nil {
			if *ruleResult == RuleAllow {
				return Decision{Effect: Allow, Reason: "Permission rule: allow"}
			}
			return Decision{Effect: Deny, Reason: "Permission rule: deny"}
		}
	}

	// Layer 4: permission mode
	effect := ModeDecide(c.Mode, cat)
	if effect == Allow {
		return Decision{Effect: Allow, Reason: fmt.Sprintf("Permission mode %s: allow", c.Mode)}
	}
	if effect == Deny {
		return Decision{Effect: Deny, Reason: fmt.Sprintf("Permission mode %s: deny", c.Mode)}
	}

	// Layer 5: ASK → HITL
	return Decision{Effect: Ask, Reason: "User confirmation required"}
}

func isPlanFile(targetPath, planPath string) bool {
	if planPath == "" || targetPath == "" {
		return false
	}
	// Try absolute path comparison
	absTarget, err1 := filepath.Abs(targetPath)
	absPlan, err2 := filepath.Abs(planPath)
	if err1 == nil && err2 == nil && absTarget == absPlan {
		return true
	}
	// Check if target ends with the plan file's relative suffix
	cleanTarget := filepath.Clean(targetPath)
	cleanPlan := filepath.Clean(planPath)
	if cleanTarget == cleanPlan {
		return true
	}
	// Base name match: LLM occasionally shortens file_path to just the base name.
	// The plan slug is randomly generated (adjective+noun+timestamp), so collision
	// with an unrelated file under the same name is extremely unlikely.
	if filepath.Base(cleanTarget) == filepath.Base(cleanPlan) {
		return true
	}
	return false
}
