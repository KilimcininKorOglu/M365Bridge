// Package codingtools provides opt-in, workspace-confined coding operations.
package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/textcut"
)

// Config controls coding tool execution and resource limits.
type Config struct {
	Enabled       bool
	WorkspaceDir  string
	Timeout       time.Duration
	MaxOutput     int64
	MaxReadBytes  int64
	MaxIterations int
}

// Tool describes a JSON-compatible callable tool.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// Result is the structured result of one tool invocation.
type Result struct {
	Tool      string `json:"tool"`
	Success   bool   `json:"success"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

// Manager validates and executes coding tools inside one canonical workspace.
type Manager struct {
	config    Config
	workspace string
}

// New creates a coding tool manager after canonicalizing its workspace.
func New(config Config) (*Manager, error) {
	if strings.TrimSpace(config.WorkspaceDir) == "" {
		return nil, errors.New("workspace directory is required")
	}
	absolute, err := filepath.Abs(config.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workspace: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return nil, errors.New("workspace must be an existing directory")
	}
	if config.Timeout <= 0 || config.MaxOutput <= 0 || config.MaxReadBytes <= 0 || config.MaxIterations <= 0 {
		return nil, errors.New("timeout and resource limits must be positive")
	}
	return &Manager{config: config, workspace: filepath.Clean(canonical)}, nil
}

// Tools returns schemas for all supported coding tools.
func (m *Manager) Tools() []Tool {
	path := map[string]any{"type": "string", "description": "Workspace-relative path"}
	command := map[string]any{"type": "string", "minLength": 1}
	return []Tool{
		tool("list_files", "List files below a workspace directory.", props(map[string]any{"path": path, "recursive": map[string]any{"type": "boolean"}}, nil)),
		tool("read_file", "Read a UTF-8 file within the workspace.", props(map[string]any{"path": path}, []string{"path"})),
		tool("write_file", "Write a UTF-8 file within the workspace.", props(map[string]any{"path": path, "content": map[string]any{"type": "string"}}, []string{"path", "content"})),
		tool("search_files", "Search file contents within the workspace.", props(map[string]any{"query": map[string]any{"type": "string", "minLength": 1}, "path": path}, []string{"query"})),
		tool("shell_command", "Run a shell command with the workspace as cwd.", props(map[string]any{"command": command}, []string{"command"})),
		tool("git_status", "Show workspace Git status.", props(nil, nil)),
		tool("git_diff", "Show the workspace Git diff.", props(map[string]any{"staged": map[string]any{"type": "boolean"}}, nil)),
		tool("git_log", "Show recent workspace Git commits.", props(map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, nil)),
		tool("run_tests", "Run a test command, defaulting to go test ./....", props(map[string]any{"command": command}, nil)),
		tool("apply_patch", "Apply a unified patch inside the workspace.", props(map[string]any{"patch": map[string]any{"type": "string", "minLength": 1}}, []string{"patch"})),
	}
}

// Execute invokes one named tool with JSON-compatible arguments.
func (m *Manager) Execute(ctx context.Context, name string, arguments map[string]any) Result {
	if !m.config.Enabled {
		return Result{Tool: name, Error: "coding tools are disabled"}
	}
	if result, handled := m.executeCommandTool(ctx, name, arguments); handled {
		return result
	}
	output, truncated, err := m.executeTextTool(ctx, name, arguments)
	return textResult(name, output, truncated, err)
}

// executeCommandTool runs the tools that shell out. handled is false when the
// name belongs to a tool that produces text instead.
func (m *Manager) executeCommandTool(ctx context.Context, name string, arguments map[string]any) (Result, bool) {
	switch name {
	case "shell_command":
		return m.shellCommandTool(ctx, name, arguments), true
	case "git_status":
		return m.command(ctx, name, []string{"git", "status", "--short"}, nil), true
	case "git_diff":
		return m.gitDiffTool(ctx, name, arguments), true
	case "git_log":
		return m.gitLogTool(ctx, name, arguments), true
	case "run_tests":
		return m.runTestsTool(ctx, name, arguments), true
	}
	return Result{}, false
}

// executeTextTool runs the tools that produce text. An unknown name lands here,
// because every command tool was already matched by executeCommandTool.
func (m *Manager) executeTextTool(ctx context.Context, name string, arguments map[string]any) (string, bool, error) {
	switch name {
	case "list_files":
		return m.listFiles(arguments)
	case "read_file":
		return m.readFile(arguments)
	case "write_file":
		output, err := m.writeFile(arguments)
		return output, false, err
	case "search_files":
		return m.searchFiles(arguments)
	case "apply_patch":
		return m.applyPatch(ctx, arguments)
	}
	return "", false, fmt.Errorf("unknown tool %q", name)
}

// textResult shapes the answer of a tool that produced text rather than running
// a process, and of a command tool that never got as far as running one.
func textResult(name, output string, truncated bool, err error) Result {
	result := Result{Tool: name, Output: output, Truncated: truncated, Success: err == nil}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// shellCommandTool runs the command line the caller supplied.
func (m *Manager) shellCommandTool(ctx context.Context, name string, arguments map[string]any) Result {
	command, err := stringArg(arguments, "command", true)
	if err != nil {
		return textResult(name, "", false, err)
	}
	return m.command(ctx, name, command, nil)
}

// gitDiffTool shows the workspace diff, staged when the caller asked for it.
func (m *Manager) gitDiffTool(ctx context.Context, name string, arguments map[string]any) Result {
	args := []string{"git", "diff"}
	if booleanArg(arguments, "staged") {
		args = append(args, "--staged")
	}
	return m.command(ctx, name, args, nil)
}

// gitLogTool shows the most recent workspace commits.
func (m *Manager) gitLogTool(ctx context.Context, name string, arguments map[string]any) Result {
	limit, err := intArg(arguments, "limit", 10, 1, 100)
	if err != nil {
		return textResult(name, "", false, err)
	}
	return m.command(ctx, name, []string{"git", "log", "--oneline", fmt.Sprintf("-%d", limit)}, nil)
}

// runTestsTool runs the caller's test command, defaulting to the Go one.
func (m *Manager) runTestsTool(ctx context.Context, name string, arguments map[string]any) Result {
	command, err := stringArg(arguments, "command", false)
	if err != nil {
		return textResult(name, "", false, err)
	}
	if command == "" {
		command = "go test ./..."
	}
	return m.command(ctx, name, command, nil)
}

// cleanRelativePath refuses an absolute path and a traversal, and normalizes
// what is left. An empty path names the workspace root.
func cleanRelativePath(path string) (string, error) {
	if path == "" {
		path = "."
	}
	if filepath.IsAbs(path) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path traversal is not allowed")
	}
	return clean, nil
}

// existingAncestor walks up from a path that does not exist yet and returns the
// nearest ancestor that does, because only an existing path can be resolved
// through its symlinks.
func existingAncestor(path string) (string, error) {
	for {
		if _, err := os.Lstat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", errors.New("no existing parent")
		}
		path = parent
	}
}

// resolve turns a caller-supplied path into a workspace path, and refuses one
// that leaves the workspace or names a protected credential file. With create
// set the path itself need not exist yet.
func (m *Manager) resolve(path string, create bool) (string, error) {
	clean, err := cleanRelativePath(path)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(m.workspace, clean)

	check := candidate
	if create {
		if check, err = existingAncestor(candidate); err != nil {
			return "", err
		}
	}

	canonical, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", err
	}
	if !contained(m.workspace, canonical) {
		return "", errors.New("path escapes workspace through a symlink")
	}
	if protected(clean) {
		return "", errors.New("access to protected credential paths is denied")
	}
	return candidate, nil
}

func contained(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func protected(path string) bool {
	p := strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
	if p == ".env" || p == "data/.env" || p == "data/tokens" || strings.HasPrefix(p, "data/tokens/") {
		return true
	}
	for part := range strings.SplitSeq(p, "/") {
		for _, word := range []string{"credential", "token", "cookie", "secret", "private_key", "private-key"} {
			if strings.Contains(part, word) {
				return true
			}
		}
		if part == "key" || strings.HasSuffix(part, ".key") {
			return true
		}
	}
	return false
}

// skipProtected tells a directory walk to leave a protected path alone: a
// directory is not descended into and a file is not reported.
func skipProtected(entry fs.DirEntry) error {
	if entry.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// listing accumulates the names one directory walk found.
type listing struct {
	manager   *Manager
	root      string
	recursive bool
	names     []string
}

// visit is the walk callback that records one entry.
func (l *listing) visit(current string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	if current == l.root {
		return nil
	}
	rel, relErr := filepath.Rel(l.manager.workspace, current)
	if relErr != nil {
		return relErr
	}
	if protected(rel) {
		return skipProtected(entry)
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	if !l.recursive && entry.IsDir() {
		l.names = append(l.names, filepath.ToSlash(rel)+"/")
		return filepath.SkipDir
	}
	if !entry.IsDir() {
		l.names = append(l.names, filepath.ToSlash(rel))
	}
	return nil
}

func (m *Manager) listFiles(a map[string]any) (string, bool, error) {
	path, err := m.resolve(optionalString(a, "path"), false)
	if err != nil {
		return "", false, err
	}
	walk := &listing{manager: m, root: path, recursive: booleanArg(a, "recursive")}
	if err := filepath.WalkDir(path, walk.visit); err != nil {
		return "", false, err
	}
	sort.Strings(walk.names)
	return bound(strings.Join(walk.names, "\n"), m.config.MaxOutput)
}

func (m *Manager) readFile(a map[string]any) (string, bool, error) {
	pathArg, err := stringArg(a, "path", true)
	if err != nil {
		return "", false, err
	}
	path, err := m.resolve(pathArg, false)
	if err != nil {
		return "", false, err
	}
	// m.resolve rejected an absolute path, a traversal, a symlink leaving the
	// workspace and every protected credential path before this point.
	// #nosec G304
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, m.config.MaxReadBytes+1))
	if err != nil {
		return "", false, err
	}
	truncated := int64(len(data)) > m.config.MaxReadBytes
	if truncated {
		data = data[:m.config.MaxReadBytes]
	}
	return string(data), truncated, nil
}

func (m *Manager) writeFile(a map[string]any) (string, error) {
	pathArg, err := stringArg(a, "path", true)
	if err != nil {
		return "", err
	}
	content, err := stringArg(a, "content", false)
	if err != nil {
		return "", err
	}
	if int64(len(content)) > m.config.MaxReadBytes {
		return "", errors.New("content exceeds write limit")
	}
	path, err := m.resolve(pathArg, true)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes", len(content)), nil
}

// search accumulates the matching lines one directory walk found.
type search struct {
	manager *Manager
	query   string
	matches []string
}

// visit is the walk callback that searches one file.
func (s *search) visit(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	rel, _ := filepath.Rel(s.manager.workspace, path)
	if protected(rel) {
		return skipProtected(entry)
	}
	if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	data, readErr := s.manager.readWalkedFile(path)
	if readErr != nil || int64(len(data)) > s.manager.config.MaxReadBytes {
		return nil
	}
	s.collectMatches(rel, data)
	return nil
}

// collectMatches records every line of one file that contains the query, with
// its line number.
func (s *search) collectMatches(rel string, data []byte) {
	for number, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, s.query) {
			s.matches = append(s.matches, fmt.Sprintf("%s:%d:%s", filepath.ToSlash(rel), number+1, line))
		}
	}
}

func (m *Manager) searchFiles(a map[string]any) (string, bool, error) {
	query, err := stringArg(a, "query", true)
	if err != nil {
		return "", false, err
	}
	root, err := m.resolve(optionalString(a, "path"), false)
	if err != nil {
		return "", false, err
	}
	walk := &search{manager: m, query: query}
	if err := filepath.WalkDir(root, walk.visit); err != nil {
		return "", false, err
	}
	return bound(strings.Join(walk.matches, "\n"), m.config.MaxOutput)
}

// readWalkedFile reads a file a directory walk found.
//
// The walk's DirEntry reports what the directory listing said, and that answer
// is already stale by the time the file is opened. So the open is checked
// against the file it actually reached: a regular file whose resolved path is
// still inside the workspace. Without that, a symlink planted between the
// listing and the read would let a search report a file from anywhere on disk.
func (m *Manager) readWalkedFile(path string) ([]byte, error) {
	// The opened file is checked below for being regular and still inside the
	// workspace, which is what makes the walk path safe to open.
	// #nosec G304
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if !contained(m.workspace, canonical) {
		return nil, errors.New("path escapes workspace through a symlink")
	}
	return io.ReadAll(io.LimitReader(file, m.config.MaxReadBytes+1))
}

func (m *Manager) applyPatch(ctx context.Context, a map[string]any) (string, bool, error) {
	patch, err := stringArg(a, "patch", true)
	if err != nil {
		return "", false, err
	}
	if int64(len(patch)) > m.config.MaxReadBytes {
		return "", false, errors.New("patch exceeds input limit")
	}
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") {
			fields := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "--- "))
			if len(fields) == 0 {
				return "", false, errors.New("patch contains an empty file path")
			}
			name := fields[0]
			if name == "/dev/null" {
				continue
			}
			name = strings.TrimPrefix(strings.TrimPrefix(name, "a/"), "b/")
			if _, err = m.resolve(name, true); err != nil {
				return "", false, fmt.Errorf("unsafe patch path: %w", err)
			}
		}
	}
	result := m.command(ctx, "apply_patch", []string{"git", "apply", "--whitespace=error", "-"}, []byte(patch))
	if !result.Success {
		return result.Output, result.Truncated, errors.New(result.Error)
	}
	return result.Output, result.Truncated, nil
}

// buildCommand turns a command literal into a process. A string is a command
// line the caller wrote and runs through the shell; a []string is an argument
// vector this package built and is executed directly.
func buildCommand(ctx context.Context, command any) (*exec.Cmd, error) {
	switch value := command.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("command is required")
		}
		return shellCommand(ctx, value), nil
	case []string:
		if len(value) == 0 {
			return nil, errors.New("command is required")
		}
		// Running a command is what this package is for, and it is off unless
		// M365_ENABLE_CODE_TOOLS turns it on. Every []string form here is built
		// from literals in Execute, never from caller text.
		// #nosec G204
		return exec.CommandContext(ctx, value[0], value[1:]...), nil
	}
	return nil, errors.New("invalid command")
}

func (m *Manager) command(ctx context.Context, tool string, command any, stdin []byte) Result {
	result := Result{Tool: tool}
	timed, cancel := context.WithTimeout(ctx, m.config.Timeout)
	defer cancel()
	cmd, buildErr := buildCommand(timed, command)
	if buildErr != nil {
		result.Error = buildErr.Error()
		return result
	}
	cmd.Dir = m.workspace
	configureProcess(cmd)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	capture := &limitedBuffer{limit: m.config.MaxOutput}
	cmd.Stdout, cmd.Stderr = capture, capture
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	if timed.Err() != nil {
		terminateProcess(cmd)
		result.TimedOut = true
		result.Error = "command timed out"
	} else if err != nil {
		result.Error = err.Error()
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	result.Output, result.Truncated = capture.String(), capture.truncated
	result.Success = err == nil && !result.TimedOut
	return result
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if int64(len(p)) > remaining {
		p = textcut.Truncate(p, int(remaining))
		b.truncated = true
	}
	_, err := b.buffer.Write(p)
	return original, err
}
func (b *limitedBuffer) String() string { return b.buffer.String() }
func bound(value string, limit int64) (string, bool, error) {
	if int64(len(value)) <= limit {
		return value, false, nil
	}
	return textcut.Truncate(value, int(limit)), true, nil
}

func tool(name, description string, schema map[string]any) Tool {
	return Tool{Name: name, Description: description, InputSchema: schema}
}
func props(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func stringArg(a map[string]any, key string, required bool) (string, error) {
	value, ok := a[key]
	if !ok {
		if required {
			return "", fmt.Errorf("%s is required", key)
		}
		return "", nil
	}
	text, ok := value.(string)
	if !ok || (required && strings.TrimSpace(text) == "") {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return text, nil
}
func optionalString(a map[string]any, key string) string {
	value, _ := stringArg(a, key, false)
	return value
}
func booleanArg(a map[string]any, key string) bool { value, _ := a[key].(bool); return value }
func intArg(a map[string]any, key string, fallback, min, max int) (int, error) {
	value, ok := a[key]
	if !ok {
		return fallback, nil
	}
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) || int(number) < min || int(number) > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", key, min, max)
	}
	return int(number), nil
}

// MarshalResult serializes a tool result as JSON.
func MarshalResult(result Result) ([]byte, error) { return json.Marshal(result) }
