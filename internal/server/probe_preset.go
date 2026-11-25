package server

// 注入自检探针（PRD v0.2 US-006）：发一条最短请求，回答「预设到底有没有进到上游」。
//
// 站内经验（linux.do 3074069#1）是「先问『你是？』，看他进没进角色」——但人肉问
// 无法自动化、也无法在换了绑定之后立刻回归。这里把同一条请求做成网关内部 RPC：
// 走**与真实请求完全相同的出站路径**（同一 PromptMode、同一预设绑定、同一 body 组装），
// 因此它测出来的结论对真实流量同样成立。
//
// 与真实请求的区别只有两点，都是刻意的：
//   - max_tokens 很小（默认 64），探针不该烧配额；
//   - 不落请求归档（不进 reqlog 的最近记录），避免探针污染面板统计。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// probeAsk 探针正文：一句最短的「你是谁」，与站内做法一致。
const probeAsk = "你是？"

// probeIdentityMarkers 官方自我介绍的指纹串：回复里出现任意一条即判定「没进角色」。
// 只做提示，不做判定处罚——探针是观测工具，不是门禁。
var probeIdentityMarkers = []string{
	"WorkBuddy",
	"CodeBuddy",
	"codebuddy",
	"腾讯",
	"Tencent",
	"AI 助手",
	"AI助手",
	"语言模型",
}

// ProbePreset 用一条最小请求验证预设是否生效。
//
// presetID 非空时必须等于该模型当前生效的预设——探针测的是**当前配置**，
// 不是「假设我绑了另一个会怎样」；后者请先绑定再探，避免探针偷偷改全局状态。
func (h *Handler) ProbePreset(model, presetID string) (map[string]any, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	_, bare := resolveModel(model)
	effective := ""
	if h.cfg.Presets != nil {
		if p, ok := h.cfg.Presets.Resolve(bare); ok {
			effective = p.ID
		}
	}
	if presetID != "" && presetID != effective {
		return nil, fmt.Errorf("preset %q is not effective for %s (effective=%q); bind it first — the probe measures the live configuration", presetID, model, effective)
	}
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages": []any{map[string]any{"role": "user", "content": probeAsk}},
		"max_tokens": 64,
		"stream":     false,
	})
	if err != nil {
		return nil, err
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	}
	rec := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(started)

	respBody, _ := io.ReadAll(rec.Body)
	reply, parsed := extractProbeReply(respBody)
	// 只有「HTTP 200 + 真拿到 choices[0].message.content + 没有官方自我介绍指纹」才算生效。
	// 少了 parsed 这一条，网关自己回的 {"error":{"code":"no_healthy_account"}} 会被当成
	// 「没出现官方话术」→ 误判成功（实测踩到过：探针在零账号实例上报 ✅）。
	applied := rec.Code == http.StatusOK && parsed && reply != "" && !containsAny(reply, probeIdentityMarkers)
	return map[string]any{
		"model":       model,
		"preset_id": effective,
		"prompt_mode": h.cfg.PromptMode,
		"status":    rec.Code,
		"applied":   applied,
		"reply":     truncateRunes(reply, 400),
		"raw":       truncateRunes(strings.TrimSpace(string(respBody)), 400),
		"latency_ms": elapsed.Milliseconds(),
		"ask":       probeAsk,
	}, nil
}

// extractProbeReply 取 choices[0].message.content；非 200 时把上游原文带回一部分，
// 让调用方直接看到 11128 之类的内容——探针失败时最需要的就是原文。
// parsed=false 表示响应体里没有 choices（网关错误 / 上游错误 / 非 JSON）。
func extractProbeReply(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &doc); err == nil && len(doc.Choices) > 0 {
		return strings.TrimSpace(doc.Choices[0].Message.Content), true
	}
	return "", false
}

func containsAny(text string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(text, n) {
			return true
		}
	}
	return false
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	return string(r[:limit]) + "…"
}
