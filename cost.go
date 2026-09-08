package main

import (
	"encoding/json"
	"fmt"
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
