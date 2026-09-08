package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type codexLog struct {
	ID, Parent, Path, Model, Effort, AgentPath string
	Spawned                                    []string
	Complete                                   bool
}

type logRecord struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string          `json:"id"`
		ForkedFrom string          `json:"forked_from_id"`
		Type       string          `json:"type"`
		Model      string          `json:"model"`
		Effort     string          `json:"effort"`
		TurnID     string          `json:"turn_id"`
		CallID     string          `json:"call_id"`
		Name       string          `json:"name"`
		Output     json.RawMessage `json:"output"`
		Source     json.RawMessage `json:"source"`
	} `json:"payload"`
}

func readLogMeta(path string) (codexLog, error) {
	file, err := os.Open(path)
	if err != nil {
		return codexLog{}, err
	}
	defer file.Close()
	var record logRecord
	if err := json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(&record); err != nil {
		return codexLog{}, err
	}
	if record.Type != "session_meta" || record.Payload.ID == "" {
		return codexLog{}, fmt.Errorf("missing session metadata")
	}
	var source struct {
		Subagent struct {
			Spawn struct {
				Parent string `json:"parent_thread_id"`
				Path   string `json:"agent_path"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	_ = json.Unmarshal(record.Payload.Source, &source)
	parent := source.Subagent.Spawn.Parent
	if parent == "" {
		parent = record.Payload.ForkedFrom
	}
	return codexLog{ID: record.Payload.ID, Parent: parent, Path: path, AgentPath: source.Subagent.Spawn.Path}, nil
}

func readLogContext(log *codexLog) error {
	file, err := os.Open(log.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	turn := ""
	spawns := map[string]bool{}
	log.Spawned = nil
	for scanner.Scan() {
		var record logRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			return fmt.Errorf("invalid session log")
		}
		switch record.Type {
		case "response_item":
			p := record.Payload
			if p.Type == "function_call" && (p.Name == "spawn_agent" || strings.HasSuffix(p.Name, ".spawn_agent")) {
				spawns[p.CallID] = true
			}
			if p.Type == "function_call_output" && spawns[p.CallID] {
				var output string
				if json.Unmarshal(p.Output, &output) != nil {
					continue
				}
				var result struct {
					ID    string          `json:"agent_id"`
					Path  string          `json:"task_name"`
					Error json.RawMessage `json:"error"`
				}
				if json.Unmarshal([]byte(output), &result) != nil {
					continue
				}
				if result.ID != "" {
					log.Spawned = append(log.Spawned, result.ID)
				} else if result.Path != "" {
					log.Spawned = append(log.Spawned, result.Path)
				} else if len(result.Error) == 0 {
					return fmt.Errorf("spawn result has no session identity")
				}
			}
		case "turn_context":
			if record.Payload.Model != "" {
				log.Model, log.Effort = record.Payload.Model, record.Payload.Effort
			}
		case "event_msg":
			if record.Payload.Type == "task_started" {
				turn, log.Complete = record.Payload.TurnID, false
			}
			if record.Payload.Type == "task_complete" && record.Payload.TurnID == turn && turn != "" {
				log.Complete = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if log.Model == "" || !validModel(log.Model) {
		return fmt.Errorf("session has no valid model")
	}
	return nil
}

func codexHome() (string, error) {
	if path := os.Getenv("CODEX_HOME"); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".codex"), err
}

func codexLogTree(home, root string, started time.Time) ([]codexLog, error) {
	if root == "" {
		return nil, fmt.Errorf("Codex did not expose a session ID")
	}
	logs := map[string]codexLog{}
	for _, directory := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(home, directory)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !entry.Type().IsRegular() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().Before(started.Add(-time.Minute)) {
				return nil
			}
			log, err := readLogMeta(path)
			if err != nil {
				return nil
			}
			if _, exists := logs[log.ID]; !exists {
				logs[log.ID] = log
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if _, exists := logs[root]; !exists {
		return nil, fmt.Errorf("Codex session log is missing")
	}
	selected := map[string]bool{root: true}
	for changed := true; changed; {
		changed = false
		for id, log := range logs {
			if !selected[id] && selected[log.Parent] {
				selected[id], changed = true, true
			}
		}
	}
	var result []codexLog
	for id := range selected {
		log := logs[id]
		if err := readLogContext(&log); err != nil {
			return nil, fmt.Errorf("session %s: %w", id, err)
		}
		result = append(result, log)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	identities := map[string]bool{}
	for _, log := range result {
		identities[log.ID] = true
		if log.AgentPath != "" {
			identities[log.AgentPath] = true
		}
	}
	for _, log := range result {
		for _, child := range log.Spawned {
			if !identities[child] {
				return nil, fmt.Errorf("a spawned descendant session log is missing")
			}
		}
	}
	return result, nil
}

type usageCounts struct {
	Input    int64 `json:"inputTokens"`
	Cached   int64 `json:"cacheReadTokens"`
	Created  int64 `json:"cacheCreationTokens"`
	Output   int64 `json:"outputTokens"`
	Total    int64 `json:"totalTokens"`
	Fallback bool  `json:"isFallback"`
}

func (u usageCounts) valid() bool {
	return u.Input >= 0 && u.Input <= 1e12 && u.Cached >= 0 && u.Cached <= 1e12 &&
		u.Created >= 0 && u.Created <= 1e12 && u.Output >= 0 && u.Output <= 1e12 &&
		u.Total == u.Input+u.Cached+u.Created+u.Output
}

func decodeUsageReport(data []byte, expected map[string]string) (float64, error) {
	var report struct {
		Sessions []struct {
			usageCounts
			ID     string                 `json:"sessionId"`
			USD    *float64               `json:"costUSD"`
			Models map[string]usageCounts `json:"models"`
		} `json:"sessions"`
		Totals struct {
			USD *float64 `json:"costUSD"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return 0, err
	}
	if len(report.Sessions) != len(expected) || len(expected) == 0 {
		return 0, fmt.Errorf("ccusage did not report every selected session")
	}
	total := 0.0
	seen := map[string]bool{}
	for _, session := range report.Sessions {
		if _, exists := expected[session.ID]; !exists || seen[session.ID] {
			return 0, fmt.Errorf("unexpected or duplicate ccusage session")
		}
		seen[session.ID] = true
		if session.USD == nil || math.IsNaN(*session.USD) || math.IsInf(*session.USD, 0) || *session.USD <= 0 || !session.valid() || len(session.Models) == 0 {
			return 0, fmt.Errorf("ccusage returned missing or invalid cost data")
		}
		var modelsTotal int64
		for model, usage := range session.Models {
			if !validModel(model) || model == "" || usage.Fallback || !usage.valid() {
				return 0, fmt.Errorf("ccusage used a fallback model or invalid usage")
			}
			modelsTotal += usage.Total
		}
		if modelsTotal != session.Total {
			return 0, fmt.Errorf("ccusage model breakdown does not match session totals")
		}
		total += *session.USD
	}
	if report.Totals.USD == nil || math.Abs(*report.Totals.USD-total) > 1e-8 || math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, fmt.Errorf("ccusage cost total does not match sessions")
	}
	return total, nil
}

func collectCost(ctx context.Context, cfg Config, root string, started time.Time, events codexEvents) *ReviewCost {
	cost := &ReviewCost{RequestedModel: cfg.Model, RequestedEffort: cfg.ReasoningEffort, ThreadID: root, CalculatedAt: time.Now().UTC()}
	home, err := codexHome()
	if err != nil {
		cost.UsageError = err.Error()
		return cost
	}
	logs, err := codexLogTree(home, root, started)
	if err != nil {
		cost.UsageError = bounded(err.Error(), 512)
		return cost
	}
	for _, log := range logs {
		cost.SessionIDs = append(cost.SessionIDs, log.ID)
		if log.ID == root {
			cost.Model, cost.Effort = log.Model, log.Effort
		}
	}
	data, version, err := runCCUsage(ctx, logs, home)
	cost.Calculator = version
	if err != nil {
		cost.UsageError = bounded(err.Error(), 512)
		return cost
	}
	cost.Report = data
	expected := map[string]string{}
	for _, log := range logs {
		expected[strings.TrimSuffix(filepath.Base(log.Path), ".jsonl")] = log.ID
		if !log.Complete {
			cost.UsageError = "a session did not finish; usage may be partial"
		}
	}
	for child := range events.children {
		found := false
		for _, id := range cost.SessionIDs {
			if id == child {
				found = true
			}
		}
		if !found {
			cost.UsageError = "a spawned session log is missing"
		}
	}
	amount, err := decodeUsageReport(data, expected)
	if err != nil {
		cost.UsageError = bounded(err.Error(), 512)
	}
	if events.err != "" {
		cost.UsageError = events.err
	}
	if !events.complete {
		cost.UsageError = "Codex did not finish; usage may be partial"
	}
	if cost.UsageError == "" {
		cost.USD = &amount
	}
	return cost
}

func runCCUsage(ctx context.Context, logs []codexLog, home string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, "ccusage", "--version")
	prepareProcessGroup(command)
	var version limitedOutput
	command.Stdout = &version
	err := cleanupProcessGroup(command, command.Run())
	if err != nil {
		return nil, "", fmt.Errorf("ccusage is required for review accounting: %w", err)
	}
	versionText := strings.TrimSpace(version.String())
	var major, minor, patch int
	if _, err := fmt.Sscanf(versionText, "ccusage %d.%d.%d", &major, &minor, &patch); err != nil ||
		version.exceeded || major != 20 || (minor == 0 && patch < 19) {
		return nil, bounded(versionText, 128), fmt.Errorf("accounting requires ccusage >=20.0.19 and <21")
	}
	dir, err := os.MkdirTemp("", "reviewctl-usage-")
	if err != nil {
		return nil, versionText, err
	}
	defer os.RemoveAll(dir)
	for _, log := range logs {
		source, err := os.Open(log.Path)
		if err != nil {
			return nil, versionText, err
		}
		target, err := os.OpenFile(filepath.Join(dir, filepath.Base(log.Path)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			source.Close()
			return nil, versionText, err
		}
		n, copyErr := io.Copy(target, io.LimitReader(source, (256<<20)+1))
		source.Close()
		closeErr := target.Close()
		if copyErr != nil || closeErr != nil || n > 256<<20 {
			return nil, versionText, fmt.Errorf("cannot snapshot session log")
		}
	}
	// Only ccusage sees this scoped home. Codex keeps its normal authentication and configuration.
	config := filepath.Join(dir, "ccusage.json")
	if err := os.WriteFile(config, []byte(`{"defaults":{"offline":false}}`), 0o600); err != nil {
		return nil, versionText, err
	}
	// Preserve the fallback speed setting used by ccusage for unclassified token events.
	if data, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "[") {
				break
			}
			if strings.HasPrefix(strings.TrimSpace(line), "service_tier") {
				if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(line+"\n"), 0o600); err != nil {
					return nil, versionText, err
				}
			}
		}
	}
	command = exec.CommandContext(ctx, "ccusage", "codex", "session", "--json", "--no-offline", "--config", config)
	prepareProcessGroup(command)
	command.Env = append(os.Environ(), "CODEX_HOME="+dir, "NO_COLOR=1")
	command.Dir = dir
	var stdout limitedOutput
	var stderr limitedOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := cleanupProcessGroup(command, command.Run()); err != nil {
		return nil, versionText, fmt.Errorf("ccusage failed: %w: %s", err, bounded(stderr.String(), 512))
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, versionText, fmt.Errorf("ccusage output exceeds 8 MiB")
	}
	if strings.TrimSpace(stderr.String()) != "" {
		return nil, versionText, fmt.Errorf("ccusage reported a warning: %s", bounded(stderr.String(), 512))
	}
	return stdout.Bytes(), versionText, nil
}

type limitedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (w *limitedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if w.Len()+n > 8<<20 {
		w.exceeded = true
		return n, nil
	}
	return w.Buffer.Write(data)
}

type codexEvents struct {
	buffer   []byte
	root     string
	complete bool
	children map[string]bool
	err      string
}

func (e *codexEvents) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		line, rest, found := bytes.Cut(data, []byte{'\n'})
		if len(e.buffer)+len(line) <= 16<<20 {
			e.buffer = append(e.buffer, line...)
		} else {
			e.err = "Codex JSONL event exceeds 16 MiB"
		}
		if found {
			e.consume(e.buffer)
			e.buffer = e.buffer[:0]
			data = rest
		} else {
			break
		}
	}
	return n, nil
}
func (e *codexEvents) consume(line []byte) {
	var event struct {
		Type   string `json:"type"`
		Thread string `json:"thread_id"`
		Item   struct {
			Type      string   `json:"type"`
			Tool      string   `json:"tool"`
			Status    string   `json:"status"`
			Receivers []string `json:"receiver_thread_ids"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &event) != nil {
		e.err = "invalid Codex JSONL"
		return
	}
	switch event.Type {
	case "thread.started":
		e.root = event.Thread
	case "turn.completed":
		e.complete = true
	case "item.completed":
		if event.Item.Type == "collab_tool_call" && event.Item.Tool == "spawn_agent" && event.Item.Status == "completed" {
			if e.children == nil {
				e.children = map[string]bool{}
			}
			for _, id := range event.Item.Receivers {
				e.children[id] = true
			}
		}
	}
}

func runCodex(ctx context.Context, cfg Config, workspace, receipt, instruction string) (*ReviewCost, error) {
	args := codexExecArgs(workspace, receipt)
	args = append(args[:len(args)-1], "--json")
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(cfg.ReasoningEffort))
	}
	args = append(args, "-")
	command := exec.CommandContext(ctx, "codex", args...)
	prepareProcessGroup(command)
	command.Dir, command.Stdin = workspace, strings.NewReader(instruction)
	var events codexEvents
	var stderr tailWriter
	command.Stdout, command.Stderr = &events, &stderr
	started := time.Now()
	err := cleanupProcessGroup(command, command.Run())
	if len(events.buffer) > 0 {
		events.consume(events.buffer)
	}
	// Accounting must survive an attempt timeout without extending the model process's lifetime.
	accountingCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cost := collectCost(accountingCtx, cfg, events.root, started, events)
	if err != nil {
		cost.USD = nil
		cost.UsageError = "Codex failed; recorded usage may be partial"
		return cost, fmt.Errorf("codex failed: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return cost, nil
}

type tailWriter []byte

func (w *tailWriter) Write(data []byte) (int, error) {
	n := len(data)
	*w = append(*w, data[max(0, n-512):]...)
	*w = (*w)[max(0, len(*w)-512):]
	return n, nil
}
