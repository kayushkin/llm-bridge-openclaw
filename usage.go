package main

import "github.com/kayushkin/llm-bridge/msg"

// UsageAggregator accumulates per-API-call token usage across a multi-turn run.
type UsageAggregator struct {
	calls     []msg.TokenUsage
	toolCalls int
}

// AddAPICall records token usage from a single OpenClaw message.
func (a *UsageAggregator) AddAPICall(usage ocUsage) {
	a.calls = append(a.calls, msg.TokenUsage{
		InputTokens:      usage.Input,
		OutputTokens:     usage.Output,
		CacheReadTokens:  usage.CacheRead,
		CacheWriteTokens: usage.CacheWrite,
		TotalTokens:      usage.Total,
	})
}

// AddToolCall increments the tool call counter.
func (a *UsageAggregator) AddToolCall() {
	a.toolCalls++
}

// Finalize builds the canonical TokenUsage and Cost from the last message.
func (a *UsageAggregator) Finalize(m jsonlMessage) (msg.TokenUsage, *msg.Cost) {
	// Sum all API call usages for aggregates.
	var usage msg.TokenUsage
	for _, call := range a.calls {
		usage.InputTokens += call.InputTokens
		usage.OutputTokens += call.OutputTokens
		usage.CacheReadTokens += call.CacheReadTokens
		usage.CacheWriteTokens += call.CacheWriteTokens
		usage.TotalTokens += call.TotalTokens
	}

	var cost *msg.Cost
	if m.Usage != nil && m.Usage.Cost != nil && m.Usage.Cost.Total > 0 {
		cost = &msg.Cost{TotalUSD: m.Usage.Cost.Total}
		if m.Usage.Cost.Input > 0 {
			cost.InputUSD = m.Usage.Cost.Input
		}
		if m.Usage.Cost.Output > 0 {
			cost.OutputUSD = m.Usage.Cost.Output
		}
	}

	return usage, cost
}

// APICallUsages returns the per-call breakdown.
func (a *UsageAggregator) APICallUsages() []msg.TokenUsage {
	return a.calls
}

// ToolCalls returns the number of tool calls observed.
func (a *UsageAggregator) ToolCalls() int {
	return a.toolCalls
}

// Reset clears the aggregator for a new turn.
func (a *UsageAggregator) Reset() {
	a.calls = a.calls[:0]
	a.toolCalls = 0
}
