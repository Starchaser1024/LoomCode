package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type GrepTool struct{}

func (t *GrepTool) Name() string { return "Grep" }

func (t *GrepTool) Description() string { return GrepDescription }

func (t *GrepTool) Category() ToolCategory { return CategoryRead }

func (t *GrepTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Regex pattern to search for"},
				"path":    map[string]any{"type": "string", "description": "Base directory to search from", "default": "."},
				"include": map[string]any{"type": "string", "description": "Glob filter for filenames (e.g. '*.py')"},
				"context": map[string]any{"type": "integer", "description": "Number of surrounding context lines before and after each match", "default": 0},
				"limit":   map[string]any{"type": "integer", "description": "Maximum number of matches to return (0 for unlimited)", "default": 0},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t *GrepTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	pattern, _ := args["pattern"].(string)
	basePath, _ := args["path"].(string)
	include, _ := args["include"].(string)
	ctxLines := intArg(args, "context", 0)
	limit := intArg(args, "limit", 0)
	if ctxLines < 0 {
		ctxLines = 0
	}
	if basePath == "" {
		basePath = "."
	}
	if pattern == "" {
		return ToolResult{Output: "Error: pattern is required", IsError: true}
	}

	info, err := os.Stat(basePath)
	if os.IsNotExist(err) {
		return ToolResult{Output: fmt.Sprintf("Error: path not found: %s", basePath), IsError: true}
	}
	if err != nil || !info.IsDir() {
		return ToolResult{Output: fmt.Sprintf("Error: path not found: %s", basePath), IsError: true}
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("Error: invalid regex: %s", err), IsError: true}
	}

	var files []string
	_ = filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if SkipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if include != "" {
			matched, _ := filepath.Match(include, info.Name())
			if !matched {
				return nil
			}
		}
		files = append(files, path)
		return nil
	})
	sort.Strings(files)

	var results []string
	totalMatches := 0

	for _, fpath := range files {
		if ctx != nil && ctx.Err() != nil {
			return ToolResult{Output: fmt.Sprintf("Error: %s", ctx.Err()), IsError: true}
		}
		lines, ok := readTextLines(fpath)
		if !ok {
			continue
		}
		rel, _ := filepath.Rel(basePath, fpath)
		relSlash := filepath.ToSlash(rel)

		if ctxLines == 0 {
			for idx, line := range lines {
				if re.MatchString(line) {
					results = append(results, fmt.Sprintf("%s:%d:%s", relSlash, idx+1, line))
					totalMatches++
					if limit > 0 && totalMatches >= limit {
						return ToolResult{Output: strings.Join(results, "\n")}
					}
				}
			}
			continue
		}

		emitted := make(map[int]bool)
		matchLineSet := make(map[int]bool)
		var matchedIndices []int
		for idx, line := range lines {
			if re.MatchString(line) {
				matchLineSet[idx] = true
				matchedIndices = append(matchedIndices, idx)
				totalMatches++
				if limit > 0 && totalMatches >= limit {
					break
				}
			}
		}
		for _, mIdx := range matchedIndices {
			start := mIdx - ctxLines
			if start < 0 {
				start = 0
			}
			end := mIdx + ctxLines
			if end >= len(lines) {
				end = len(lines) - 1
			}
			for i := start; i <= end; i++ {
				if emitted[i] {
					continue
				}
				emitted[i] = true
				sep := "-"
				if matchLineSet[i] {
					sep = ":"
				}
				results = append(results, fmt.Sprintf("%s%s%d%s%s", relSlash, sep, i+1, sep, lines[i]))
			}
		}
		if limit > 0 && totalMatches >= limit {
			break
		}
	}

	if len(results) == 0 {
		return ToolResult{Output: "No matches found."}
	}
	return ToolResult{Output: strings.Join(results, "\n")}
}

func readTextLines(path string) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanner.Err() != nil {
		return nil, false
	}
	return lines, true
}
