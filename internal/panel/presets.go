package panel

// 提示词预设与模型绑定（PRD v0.2 US-002 / US-003 / US-009）。
//
// 这些接口只碰预设库的两个文件（presets.json / bindings.json），不触发任何上游调用；
// 因此「读」可以安全地被面板轮询，「写」也只是落盘 + 原子快照。
// 预设库为 nil（未注入）时统一返回 501，与面板其它可选子系统同一语义。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
)

// presetsState 面板一次读到的完整状态（预设 + 绑定 + 目录 + 可选模型清单）。
func (p *Panel) presetsState() (map[string]any, bool) {
	if p.cfg.Presets == nil {
		return nil, false
	}
	list := p.cfg.Presets.List()
	items := make([]map[string]any, 0, len(list))
	for _, pr := range list {
		items = append(items, map[string]any{
			"id": pr.ID,
			"name": pr.Name,
			"kind": string(pr.Kind),
			"mode": string(pr.Mode),
			"text": pr.Text,
			"tags": pr.Tags,
			"source": string(pr.Source),
			"enabled": pr.Enabled,
			"bytes": prompt.ByteLen(pr.Text),
			"updated_at": pr.UpdatedAt,
		})
	}
	return map[string]any{
		"dir": p.cfg.Presets.Dir(),
		"presets": items,
		"bindings": p.cfg.Presets.Bindings(),
		"kinds": []string{string(prompt.KindSystem), string(prompt.KindPersona), string(prompt.KindDegraded), string(prompt.KindEncoding)},
		"modes": []string{string(prompt.ModeRaw), string(prompt.ModeCompose), string(prompt.ModePurge)},
		"max_bytes": prompt.MaxPresetBytes,
	}, true
}

func (p *Panel) presetsList(w http.ResponseWriter, _ *http.Request) {
	state, ok := p.presetsState()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "preset store not configured"})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (p *Panel) presetsSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Presets == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "preset store not configured"})
		return
	}
	var body prompt.Preset
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, prompt.MaxPresetBytes+4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	saved, err := p.cfg.Presets.Upsert(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preset": saved})
}

func (p *Panel) presetsDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Presets == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "preset store not configured"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "preset id is required"})
		return
	}
	err := p.cfg.Presets.Delete(id)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var inUse *prompt.InUseError
	if errors.As(err, &inUse) {
		// 409：预设仍被模型绑定引用；body 点名哪些模型，前端据此直接跳绑定页。
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "models": inUse.Models})
		return
	}
	if errors.Is(err, prompt.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "preset not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

// presetsImport 接受社区格式：agent-purge / keysmith / dsh-breach / pi-web / 原生数组。
// 请求体可以是 {payload: <object|array>}，也可以直接给 object/array（两者都兼容）。
func (p *Panel) presetsImport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Presets == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "preset store not configured"})
		return
	}
	raw, err := readAllLimited(w, r, prompt.MaxPresetBytes*4)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		if inner, ok := probe["payload"]; ok {
			raw = inner
		}
	}
	presets, err := prompt.ParseImport(raw, "imported")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ids := make([]string, 0, len(presets))
	for _, pr := range presets {
		if _, err := p.cfg.Presets.Upsert(pr); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "imported": ids})
			return
		}
		ids = append(ids, pr.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": ids})
}

func readAllLimited(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, errors.New("payload too large or unreadable")
	}
	return raw, nil
}

func (p *Panel) promptBindingsGet(w http.ResponseWriter, _ *http.Request) {
	state, ok := p.presetsState()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "preset store not configured"})
		return
	}
	// 绑定表已在 state 里；模型清单由 /panel/api/models 提供（本接口不重复聚合，避免两次上游探测）。
	writeJSON(w, http.StatusOK, state)
}

func (p *Panel) promptBindingsSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Presets == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "preset store not configured"})
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	bindings := map[string]string{}
	if raw, ok := body["bindings"]; ok {
		if err := json.Unmarshal(raw, &bindings); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bindings must be an object of model -> preset id"})
			return
		}
	}
	if err := p.cfg.Presets.SetBindings(bindings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bindings": p.cfg.Presets.Bindings()})
}

// promptProbe 「你是？」自检（PRD v0.2 US-006）：确认注入是否真的进到上游。
// 真正的探测请求由 main 注入（需要上游客户端与选号），面板只做转发与错误映射。
func (p *Panel) promptProbe(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Presets == nil || p.cfg.PromptProbe == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "prompt probe not configured"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	model, _ := body["model"].(string)
	presetID, _ := body["preset_id"].(string)
	if strings.TrimSpace(model) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model is required"})
		return
	}
	result, err := p.cfg.PromptProbe(model, presetID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
