package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageAccounting(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"root", ""}, {"child", "root"}, {"grandchild", "child"}, {"unrelated", ""}} {
		data := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"source":{"subagent":{"thread_spawn":{"parent_thread_id":%q}}}}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"turn_context","payload":{"model":"gpt-test","effort":"low"}}
{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"failed-spawn"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"failed-spawn","output":"spawn failed"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}
`, pair[0], pair[1])
		if err := os.WriteFile(filepath.Join(home, "sessions", pair[0]+".jsonl"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := codexLogTree(home, "root", time.Now())
	if err != nil || len(logs) != 3 {
		t.Fatalf("tree: %+v, %v", logs, err)
	}
	expected := map[string]string{}
	var rows []string
	for _, log := range logs {
		if log.ID == "unrelated" || log.Model != "gpt-test" || log.Effort != "low" || !log.Complete {
			t.Fatalf("wrong session: %+v", log)
		}
		expected[log.ID] = log.ID
		rows = append(rows, fmt.Sprintf(`{"sessionId":%q,"inputTokens":10,"cacheReadTokens":20,"outputTokens":3,"totalTokens":33,"costUSD":0.1,"models":{"gpt-test":{"inputTokens":10,"cacheReadTokens":20,"outputTokens":3,"totalTokens":33,"isFallback":false}}}`, log.ID))
	}
	report := `{"sessions":[` + strings.Join(rows, ",") + `],"totals":{"costUSD":0.3}}`
	amount, err := decodeUsageReport([]byte(report), expected)
	if err != nil || amount < 0.29999 || amount > 0.30001 {
		t.Fatalf("cost=%v err=%v", amount, err)
	}
	for _, invalid := range []string{
		strings.ReplaceAll(report, `"isFallback":false`, `"isFallback":true`),
		strings.Replace(report, `"totalTokens":33`, `"totalTokens":34`, 1),
		strings.Replace(report, `"costUSD":0.1`, `"costUSD":0`, 1),
		strings.Replace(report, `"costUSD":0.3`, `"costUSD":0.4`, 1),
		strings.Replace(report, `"sessionId":"root"`, `"sessionId":"unrelated"`, 1),
	} {
		if _, err := decodeUsageReport([]byte(invalid), expected); err == nil {
			t.Fatalf("accepted invalid report: %s", invalid)
		}
	}
	expected["missing"] = "missing"
	if _, err := decodeUsageReport([]byte(report), expected); err == nil {
		t.Fatal("accepted missing descendant")
	}
	file, err := os.OpenFile(logs[0].Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"next\"}}\n")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := readLogContext(&logs[0]); err != nil || logs[0].Complete {
		t.Fatalf("unfinished session accepted: %+v %v", logs[0], err)
	}
	for _, identity := range []string{`{"agent_id":"missing-grandchild"}`, `{"task_name":"/root/child/missing-grandchild"}`} {
		output, _ := json.Marshal(identity)
		data := fmt.Sprintf(`{"type":"session_meta","payload":{"id":"child","forked_from_id":"root"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"turn_context","payload":{"model":"gpt-test","effort":"low"}}
{"type":"response_item","payload":{"type":"function_call","name":"spawn_agent","call_id":"spawn"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"spawn","output":%s}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}
`, output)
		if err := os.WriteFile(filepath.Join(home, "sessions", "child.jsonl"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := codexLogTree(home, "root", time.Now()); err == nil || !strings.Contains(err.Error(), "descendant") {
			t.Fatalf("accepted missing nested spawn %s: %v", identity, err)
		}
	}
}

func TestCostSignatureAndDurableAccounting(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		want   string
	}{{0.10910315, "0.1"}, {0.16, "0.2"}, {0.04, "0.0"}, {7.0, "7.0"}} {
		cost := ReviewCost{Model: "gpt-test", USD: &tc.amount}
		if got := cost.signature(); got != "Model: gpt-test (~$"+tc.want+")" || *cost.USD != tc.amount {
			t.Fatalf("rounding %v: %s", tc.amount, got)
		}
	}
	amount := 7.0
	cost := &ReviewCost{Model: "gpt-test", Effort: "low", USD: &amount}
	if cost.signature() != "Model: gpt-test low (~$7.0)" {
		t.Fatal(cost.signature())
	}
	unavailable := ReviewCost{Model: "gpt-test", Effort: "high"}
	if got := unavailable.signature(); got != "Model: gpt-test high (cost unavailable)" {
		t.Fatal(got)
	}
	body, err := reviewSignatureBody("Findings\n\n<!-- original marker -->", cost.signature())
	if err != nil {
		t.Fatal(err)
	}
	if again, err := reviewSignatureBody(body, cost.signature()); err != nil || again != body {
		t.Fatalf("non-idempotent signature: %q %v", again, err)
	}
	if _, err := reviewSignatureBody(signatureEnd+signatureStart, cost.signature()); err == nil {
		t.Fatal("accepted malformed markers")
	}
	store := openTestStore(t)
	ctx := context.Background()
	// Exercise migration from the previous schema with an existing history row.
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	old := Attempt{PullRequest: pr, StartedAt: time.Now(), FinishedAt: time.Now()}
	if err := store.Finish(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE history DROP COLUMN cost_json`); err != nil {
		t.Fatal(err)
	}
	var path string
	var seq int
	var name string
	if err := store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old.Success, old.Cost, old.ReviewID = true, cost, "42"
	if err := store.Finish(ctx, old); err != nil {
		t.Fatal(err)
	}
	old.Recovered = true
	if err := store.Finish(ctx, old); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Status(ctx, 3)
	if err != nil || len(history) != 3 || history[1].Cost == nil || *history[1].Cost.USD != 7 || history[2].Cost != nil {
		t.Fatalf("history: %+v %v", history, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM review_signatures`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("signature count %d: %v", count, err)
	}
}

func TestReviewModelConfiguration(t *testing.T) {
	base := "harness: codex\npublish: true\ntrusted_authors: [alice]\nrepositories:\n  - provider: github\n    repository: acme/service\n"
	cfg, err := DecodeConfig(strings.NewReader(base + "model: gpt-test\nreasoning_effort: low\n"))
	if err != nil || cfg.Model != "gpt-test" || cfg.ReasoningEffort != "low" {
		t.Fatalf("model configuration: %+v %v", cfg, err)
	}
	for _, bad := range []string{"model: 'bad model'\n", "reasoning_effort: bogus\n"} {
		if _, err := DecodeConfig(strings.NewReader(base + bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// Opt in with an existing, completed probe thread. No model or GitHub calls are made.
func TestLiveCCUsage(t *testing.T) {
	root := os.Getenv("REVIEWCTL_LIVE_USAGE_ROOT")
	if root == "" {
		t.Skip("set REVIEWCTL_LIVE_USAGE_ROOT to verify installed ccusage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cost := collectCost(ctx, Config{}, root, time.Now().Add(-24*time.Hour), codexEvents{complete: true})
	if cost.USD == nil || cost.UsageError != "" || len(cost.SessionIDs) < 2 {
		t.Fatalf("accounting failed: %+v", cost)
	}
	data, _ := json.Marshal(cost)
	t.Log(string(data))
}
