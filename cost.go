package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type ReviewCost struct {
	RequestedModel  string          `json:"requested_model,omitempty"`
	RequestedEffort string          `json:"requested_effort,omitempty"`
	Model           string          `json:"model,omitempty"`
	Effort          string          `json:"reasoning_effort,omitempty"`
	ThreadID        string          `json:"thread_id,omitempty"`
	SessionIDs      []string        `json:"session_ids,omitempty"`
	USD             *float64        `json:"estimated_api_cost_usd"`
	UsageError      string          `json:"usage_error,omitempty"`
	CalculatedAt    time.Time       `json:"calculated_at"`
	Calculator      string          `json:"calculator,omitempty"`
	Report          json.RawMessage `json:"report,omitempty"`
}

func (c ReviewCost) signature() string {
	model := c.Model
	if model == "" {
		model = "not exposed"
	}
	if c.Effort != "" && validModel(c.Effort) {
		model += " " + c.Effort
	}
	if c.USD == nil {
		return fmt.Sprintf("Model: %s (cost unavailable)", model)
	}
	return fmt.Sprintf("Model: %s (~$%.1f)", model, *c.USD)
}

func (c ReviewCost) githubSignature(formatTokens func(int64) string) string {
	if c.Model == "" || !validModel(c.Model) {
		return ""
	}
	fields := []string{c.Model}
	if c.Effort != "" && validModel(c.Effort) {
		fields = append(fields, c.Effort)
	}
	if c.USD != nil {
		fields = append(fields, reviewTokenFields(c.Report, formatTokens)...)
		usd := strconv.FormatFloat(*c.USD, 'g', 2, 64)
		if rounded, err := strconv.ParseFloat(usd, 64); err == nil {
			usd = strconv.FormatFloat(rounded, 'f', -1, 64)
		}
		fields = append(fields, "usd="+usd)
	}
	return "<sub>🤖 " + strings.Join(fields, " ") + "</sub>"
}

func reviewTokenFields(data json.RawMessage, formatTokens func(int64) string) []string {
	var report struct {
		Sessions []struct {
			Input   *int64 `json:"inputTokens"`
			Cached  *int64 `json:"cacheReadTokens"`
			Created int64  `json:"cacheCreationTokens"`
			Output  *int64 `json:"outputTokens"`
		} `json:"sessions"`
	}
	if json.Unmarshal(data, &report) != nil || len(report.Sessions) == 0 {
		return nil
	}
	totals := [3]int64{}
	missing := [3]bool{}
	for _, session := range report.Sessions {
		for i, count := range [3]*int64{session.Input, session.Cached, session.Output} {
			if count == nil {
				missing[i] = true
			} else {
				totals[i] += *count
			}
		}
		totals[0] += session.Created
	}
	var fields []string
	for i, key := range [3]string{"in", "cached", "out"} {
		if missing[i] {
			continue
		}
		fields = append(fields, key+"="+formatTokens(totals[i]))
	}
	return fields
}

func roundedTokenCount(count int64) string {
	return strconv.FormatInt((count+500)/1000, 10) + "k"
}

func previousTokenCount(count int64) string {
	if count < 1000 {
		return strconv.FormatInt(count, 10)
	}
	value := fmt.Sprintf("%d.%03d", count/1000, count%1000)
	return strings.TrimRight(strings.TrimRight(value, "0"), ".") + "k"
}

func validModel(value string) bool {
	if len(value) > 120 || strings.TrimSpace(value) != value {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._:/", ch)) {
			return false
		}
	}
	return true
}

func encodeCost(cost *ReviewCost) (string, error) {
	if cost == nil {
		return "", nil
	}
	data, err := json.Marshal(cost)
	return string(data), err
}
