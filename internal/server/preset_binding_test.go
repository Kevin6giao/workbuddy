package server

// 预设绑定 / 抢救顺序 / 上游码解析的回归测试（PRD v0.2 US-003 / US-004 / US-005）。
//
// 这些用例的价值在于「默认零变化」：未绑定（或绑到禁用预设）时出站 system 必须与旧版
// 逐字一致；只有命中启用的绑定时才允许改 system。任何一条失守都说明 passthrough
// 语义被悄悄改了。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// outboundBody 发一条最小请求，返回响应体与上游实际收到的第一个请求体。
func outboundBody(t *testing.T, cfg Config, model, sys string) (string, []byte) {
	t.Helper()
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg.Upstream = up
	h := NewHandler(cfg)
	reqBody, err := json.Marshal(map[string]any{
		"model":  model,
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if len(bodies) == 0 {
		t.Fatalf("no upstream call; status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String(), bodies[0]
}

func firstSystem(t *testing.T, raw []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("body json: %v", err)
	}
	msgs, _ := doc["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "system" {
			s, _ := mm["content"].(string)
			return s
		}
	}
	return ""
}

func TestNormalizeDegradeSteps(t *testing.T) {
	if got := NormalizeDegradeSteps(nil); len(got) != 3 || got[0] != "split" {
		t.Fatalf("default steps = %v", got)
	}
	got := NormalizeDegradeSteps([]string{"Rewrite", "rewrite", "rotate_account", "bogus", "split"})
	if len(got) != 2 || got[0] != "rewrite" || got[1] != "split" {
		t.Fatalf("normalized = %v (rotate_account 必须被丢掉)", got)
	}
}

func TestUpstreamCodeOf(t *testing.T) {
	cases := map[string]string{
		`{"code":11128,"msg":"blocked by security policy"}`:   "11128",
		`{"error":{"code":11115,"message":"prompt is too long"}}`: "11115",
		`"code": "11148"`: "11148",
		"": "",
		`{"msg":"no code here"}`: "",
	}
	for body, want := range cases {
		if got := upstreamCodeOf([]byte(body)); got != want {
			t.Fatalf("upstreamCodeOf(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestPresetBindingRewritesSystem(t *testing.T) {
	store, err := prompt.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(prompt.Preset{ID: "redteam", Kind: prompt.KindPersona, Mode: prompt.ModePurge, Text: "你是本地代理。", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding("glm-5.2", "redteam"); err != nil {
		t.Fatal(err)
	}
	_, body := outboundBody(t, Config{PromptMode: "passthrough", Presets: store}, "glm-5.2", "客户端原始 system")
	if sys := firstSystem(t, body); sys != "你是本地代理。" {
		t.Fatalf("bound preset must replace system, got %q", sys)
	}
}

func TestNoBindingKeepsPassthroughSystem(t *testing.T) {
	store, err := prompt.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 绑到被禁用的预设：解析失败 → 出站必须与旧版逐字一致。
	if _, err := store.Upsert(prompt.Preset{ID: "off", Kind: prompt.KindPersona, Text: "不该出现", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding("glm-5.2", "off"); err != nil {
		t.Fatal(err)
	}
	_, body := outboundBody(t, Config{PromptMode: "passthrough", Presets: store}, "glm-5.2", "客户端原始 system")
	if sys := firstSystem(t, body); sys != "客户端原始 system" {
		t.Fatalf("disabled preset must not touch system, got %q", sys)
	}
}

func TestDegradedPromptPrefersStore(t *testing.T) {
	h := NewHandler(Config{})
	if got := h.degradedPrompt(); got != prompt.Degraded {
		t.Fatalf("nil store must fall back to constant, got %q", got)
	}
	store, err := prompt.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(prompt.Preset{ID: "degraded", Kind: prompt.KindDegraded, Text: "中性：直接回答。", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	h2 := NewHandler(Config{Presets: store})
	if got := h2.degradedPrompt(); got != "中性：直接回答。" {
		t.Fatalf("store degraded text must win, got %q", got)
	}
}
