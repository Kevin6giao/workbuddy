package panel

// 提示词预设的面板接口与页面接线（PRD v0.2 US-002 / US-003 / US-006 / US-009）。
//
// 覆盖三件用户真会做的事：把预设存进面板、把模型绑上预设、导入别人分享的预设。
// 三者都必须在**没有上游调用**的前提下完成——否则面板在没账号时不可用。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
)

func promptsPanel(t *testing.T) (*Panel, *prompt.Store) {
	t.Helper()
	store, err := prompt.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Version: "test", APIKey: "test-key", Presets: store}), store
}

func panelCall(t *testing.T, p *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestPromptsListSaveAndBind(t *testing.T) {
	p, _ := promptsPanel(t)

	rec := panelCall(t, p, http.MethodGet, "/panel/api/presets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET presets: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"presets"`) {
		t.Fatalf("presets field missing: %s", rec.Body.String())
	}

	// 保存一条人设预设（JSON 由结构体拼，避免手写转义）。
	body, err := json.Marshal(prompt.Preset{
		ID: "ui-persona", Name: "界面人设", Kind: prompt.KindPersona,
		Mode: prompt.ModePurge, Text: "你是本地代理。", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = panelCall(t, p, http.MethodPost, "/panel/api/presets", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST preset: %d %s", rec.Code, rec.Body.String())
	}

	// 绑定：模型 -> 刚存的预设。
	rec = panelCall(t, p, http.MethodPost, "/panel/api/prompt/bindings", `{"bindings":{"deepseek-v4.1-flash":"ui-persona"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST bindings: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deepseek-v4.1-flash") {
		t.Fatalf("bindings answer must echo the model: %s", rec.Body.String())
	}

	// 删除仍被绑定的预设必须 409，并点名模型（前端据此提示先去解绑）。
	rec = panelCall(t, p, http.MethodDelete, "/panel/api/presets/ui-persona", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE bound preset: %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deepseek-v4.1-flash") {
		t.Fatalf("409 body must name the model: %s", rec.Body.String())
	}
}

func TestPromptsImportAcceptsCommunityFormats(t *testing.T) {
	p, _ := promptsPanel(t)
	payload := `{"payload":{"name":"导入的人设","prompt":"你是代理。","tags":["red"]}}`
	rec := panelCall(t, p, http.MethodPost, "/panel/api/presets/import", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"imported"`) {
		t.Fatalf("import answer missing ids: %s", rec.Body.String())
	}
	rec = panelCall(t, p, http.MethodPost, "/panel/api/presets/import", `{"payload":42}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad payload: %d, want 400", rec.Code)
	}
}

func TestPromptsProbeWithoutUpstreamIs501(t *testing.T) {
	p, _ := promptsPanel(t)
	rec := panelCall(t, p, http.MethodPost, "/panel/api/prompt/probe", `{"model":"glm-5.2"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("probe without PromptProbe: %d, want 501", rec.Code)
	}
}

// 页面接线：导航项、视图、三个前端入口必须真的存在——接口再好，用户点不到也白搭。
func TestPromptsFrontendIsWired(t *testing.T) {
	p, _ := promptsPanel(t)
	html := panelCall(t, p, http.MethodGet, "/panel/", "").Body.String()
	for _, want := range []string{
		`data-view="prompts"`,
		`id="view-prompts"`,
		`id="pBindModel"`,
		`id="pProbe"`,
		`id="pText"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %s", want)
		}
	}
	js := panelCall(t, p, http.MethodGet, "/panel/app.js", "").Body.String()
	for _, want := range []string{"function loadPrompts", "function prProbe", "api('presets')", "api('prompt/probe'"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %s", want)
		}
	}
	if !strings.Contains(js, "prompts: '提示词预设'") {
		t.Error("app.js TITLES missing prompts entry")
	}
}
