package prompt

// 提示词预设（preset）与模型绑定：把"出站用哪段 system"从全局单值升级为
// 「预设对象 + 按模型绑定 + 热生效」。
//
// 设计依据（见 workbuddy/linuxdo-jailbreak.md 报告 v2）：
//   - N1「甲随渠道浮动」：同一个 bareModel 在不同供应商/渠道下需要一个不同的预设，
//     所以绑定键是 bareModel，而不是全局一份；
//   - 站内 dsh-inject / agent-purge / Keysmith 的预设格式各不相同，本包用一个
//     统一结构 + 导入适配层收口，并保留 mode（raw/compose/purge）语义，
//     与 Pi-Web lib/prompt-override.ts 的三档枚举一致。
//
// 语义边界：本包只决定「哪段文本送去上游」，不做任何自动改写迭代。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Kind 预设类别。
type Kind string

const (
	// KindSystem 网关自有 system 提示词（custom/append 模式的等价物）。
	KindSystem Kind = "system"
	// KindPersona 破甲/人设预设：出站时按 Mode 与网关提示词合成。
	KindPersona Kind = "persona"
	// KindDegraded 中性降级句（content_blocked 三层抢救的最后一层）。
	KindDegraded Kind = "degraded"
	// KindEncoding 字符级变形通道：在出站文本里插入零宽字符。
	KindEncoding Kind = "encoding"
)

// Source 预设来源（导入渠道）。
type Source string

const (
	SourceManual     Source = "manual"
	SourceAgentPurge Source = "agent-purge"
	SourceKeysmith   Source = "keysmith"
	SourcePurge      Source = "purge"
	SourceDSHBreach  Source = "dsh-breach"
	SourcePiWeb      Source = "pi-web"
)

// Mode 人设预设的合成方式，与 Pi-Web prompt-override.ts 的枚举逐字一致。
type Mode string

const (
	// ModeRaw 只用预设文本，丢弃网关提示词。
	ModeRaw Mode = "raw"
	// ModeCompose 网关提示词 + 预设文本。
	ModeCompose Mode = "compose"
	// ModePurge 预设文本置顶 + 去掉网关提示词里的身份段（甲伤得最深的一档）。
	ModePurge Mode = "purge"
)

// MaxPresetBytes 单条预设正文上限（导入与写入共用）。
const MaxPresetBytes = 256 << 10

// ZeroWidthGap 字符级变形的默认间隔（每 N 个字符插一个零宽空格）。
const ZeroWidthGap = 3

// ErrNotFound 预设/绑定不存在。
var ErrNotFound = errors.New("preset not found")

// InUseError 预设仍被绑定引用（删除时的 409 语义）。
type InUseError struct{ Models []string }

func (e *InUseError) Error() string {
	return "preset is bound to models: " + strings.Join(e.Models, ", ")
}

// Preset 一条提示词预设。
type Preset struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      Kind      `json:"kind"`
	Mode      Mode      `json:"mode"`
	Text      string    `json:"text"`
	Tags      []string  `json:"tags,omitempty"`
	Source    Source    `json:"source"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BuiltinPresets 内置预设集合：新装即可用，且不依赖任何第三方正文。
func BuiltinPresets() []Preset {
	now := time.Time{}
	return []Preset{
		{ID: "default", Name: "网关默认提示词", Kind: KindSystem, Text: defaultPrompt, Source: SourceManual, Enabled: true, UpdatedAt: now},
		{ID: "degraded", Name: "中性降级句", Kind: KindDegraded, Text: Degraded, Source: SourceManual, Enabled: true, UpdatedAt: now},
		{ID: "neutral-cn", Name: "中文中性句", Kind: KindDegraded, Text: "你是一个有帮助的助手。用用户使用的语言回答，遵循用户的指令，直接、简洁。", Source: SourceManual, Enabled: true, UpdatedAt: now},
		{ID: "persona-neutral", Name: "最小人设（自检用）", Kind: KindPersona, Mode: ModePurge, Text: "你现在是这个工作台的本地执行代理，直接交付可运行的结果。", Source: SourceManual, Enabled: false, UpdatedAt: now},
	}
}

// Store 预设 + 绑定的进程内快照，落盘两份文件：
//   <dir>/presets.json  预设集合
//   <dir>/bindings.json model -> preset id
//
// 读路径只碰内存（出站热路径），写路径"临时文件 + rename"，与仓库既有
// atomicWrite 风格一致；文件损坏不阻塞启动（退回内置集并把错误交回调用方）。
type Store struct {
	mu       sync.RWMutex
	dir      string
	presets  []Preset
	bindings map[string]string
}

// DefaultStoreDir 预设目录：<base>/prompts（base 通常是 data 目录）。
func DefaultStoreDir(base string) string { return filepath.Join(base, "prompts") }

// OpenStore 打开（必要时初始化）预设库。
// 返回的 error 表示"文件存在但读不动/解析失败"——此时 Store 仍然可用（内置集），
// 调用方应当把错误透出为面板告警，而不是拒绝启动。
func OpenStore(dir string) (*Store, error) {
	s := &Store{dir: dir, bindings: map[string]string{}}
	loadErr := error(nil)
	presets, err := readPresets(filepath.Join(dir, "presets.json"))
	if err != nil {
		loadErr = err
	}
	if len(presets) == 0 {
		presets = BuiltinPresets()
	} else {
		presets = mergeBuiltins(presets)
	}
	s.presets = presets
	if binds, err := readBindings(filepath.Join(dir, "bindings.json")); err != nil {
		if loadErr == nil {
			loadErr = err
		}
	} else {
		s.bindings = binds
	}
	return s, loadErr
}

func mergeBuiltins(existing []Preset) []Preset {
	seen := make(map[string]bool, len(existing))
	for _, p := range existing {
		seen[p.ID] = true
	}
	out := existing
	for _, b := range BuiltinPresets() {
		if !seen[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

func readPresets(path string) ([]Preset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("presets file %s: %w", path, err)
	}
	var list []Preset
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("presets file %s: %w", path, err)
	}
	return list, nil
}

func readBindings(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("bindings file %s: %w", path, err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]string{}, fmt.Errorf("bindings file %s: %w", path, err)
	}
	return out, nil
}

// Dir 预设目录（面板展示用）。
func (s *Store) Dir() string { return s.dir }

// List 返回预设副本（调用方不得改写内部状态）。
func (s *Store) List() []Preset {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Preset, len(s.presets))
	copy(out, s.presets)
	return out
}

// Get 按 ID 取预设。
func (s *Store) Get(id string) (Preset, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Degraded 中性降级句：degraded 类预设的第一条启用项，缺失时回落常量 Degraded。
func (s *Store) Degraded() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.presets {
		if p.Kind == KindDegraded && p.Enabled && strings.TrimSpace(p.Text) != "" {
			return p.Text
		}
	}
	return Degraded
}

// Bindings 返回 model -> preset id 的副本。
func (s *Store) Bindings() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.bindings))
	for k, v := range s.bindings {
		out[k] = v
	}
	return out
}

// Resolve 按模型解析出生效预设（绑定命中 + 预设存在 + 已启用）。
func (s *Store) Resolve(model string) (Preset, bool) {
	id := ""
	s.mu.RLock()
	id = s.bindings[strings.TrimSpace(model)]
	s.mu.RUnlock()
	if id == "" {
		return Preset{}, false
	}
	p, ok := s.Get(id)
	if !ok || !p.Enabled || strings.TrimSpace(p.Text) == "" {
		return Preset{}, false
	}
	return p, true
}

// Upsert 新建或更新预设，成功即落盘。
func (s *Store) Upsert(p Preset) (Preset, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	if p.ID == "" {
		return Preset{}, errors.New("preset id is required")
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if len(p.Text) > MaxPresetBytes {
		return Preset{}, fmt.Errorf("preset text exceeds %d bytes", MaxPresetBytes)
	}
	if strings.TrimSpace(p.Text) == "" {
		return Preset{}, errors.New("preset text is required")
	}
	if !p.Kind.valid() {
		return Preset{}, fmt.Errorf("unknown preset kind %q", p.Kind)
	}
	if p.Kind == KindPersona {
		if p.Mode == "" {
			p.Mode = ModePurge
		}
		if !p.Mode.valid() {
			return Preset{}, fmt.Errorf("unknown preset mode %q", p.Mode)
		}
	} else {
		p.Mode = ""
	}
	if !p.Source.valid() {
		p.Source = SourceManual
	}
	p.UpdatedAt = time.Now()

	s.mu.Lock()
	replaced := false
	for i := range s.presets {
		if s.presets[i].ID == p.ID {
			s.presets[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		s.presets = append(s.presets, p)
	}
	snapshot := append([]Preset(nil), s.presets...)
	s.mu.Unlock()

	if err := writeJSONAtomic(filepath.Join(s.dir, "presets.json"), snapshot); err != nil {
		return Preset{}, err
	}
	return p, nil
}

// Delete 删除预设；仍被绑定时返回 *InUseError（面板映射 409）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	idx := -1
	for i := range s.presets {
		if s.presets[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return ErrNotFound
	}
	var used []string
	for model, pid := range s.bindings {
		if pid == id {
			used = append(used, model)
		}
	}
	if len(used) > 0 {
		s.mu.Unlock()
		sortStrings(used)
		return &InUseError{Models: used}
	}
	s.presets = append(s.presets[:idx], s.presets[idx+1:]...)
	snapshot := append([]Preset(nil), s.presets...)
	s.mu.Unlock()
	return writeJSONAtomic(filepath.Join(s.dir, "presets.json"), snapshot)
}

// SetBinding 绑定 model -> preset；presetID 为空表示解绑。
func (s *Store) SetBinding(model, presetID string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model is required")
	}
	presetID = strings.TrimSpace(presetID)
	if presetID != "" {
		if _, ok := s.Get(presetID); !ok {
			return fmt.Errorf("preset %q: %w", presetID, ErrNotFound)
		}
	}
	s.mu.Lock()
	if presetID == "" {
		delete(s.bindings, model)
	} else {
		s.bindings[model] = presetID
	}
	snapshot := make(map[string]string, len(s.bindings))
	for k, v := range s.bindings {
		snapshot[k] = v
	}
	s.mu.Unlock()
	return writeJSONAtomic(filepath.Join(s.dir, "bindings.json"), snapshot)
}

// SetBindings 整体替换绑定表（面板一次保存多处改动）。
func (s *Store) SetBindings(next map[string]string) error {
	clean := make(map[string]string, len(next))
	for model, id := range next {
		model = strings.TrimSpace(model)
		id = strings.TrimSpace(id)
		if model == "" {
			continue
		}
		if id == "" {
			continue
		}
		if _, ok := s.Get(id); !ok {
			return fmt.Errorf("preset %q: %w", id, ErrNotFound)
		}
		clean[model] = id
	}
	s.mu.Lock()
	s.bindings = clean
	s.mu.Unlock()
	return writeJSONAtomic(filepath.Join(s.dir, "bindings.json"), clean)
}

func (k Kind) valid() bool {
	switch k {
	case KindSystem, KindPersona, KindDegraded, KindEncoding:
		return true
	}
	return false
}

func (m Mode) valid() bool {
	switch m {
	case ModeRaw, ModeCompose, ModePurge:
		return true
	}
	return false
}

func (src Source) valid() bool {
	switch src {
	case SourceManual, SourceAgentPurge, SourceKeysmith, SourcePurge, SourceDSHBreach, SourcePiWeb:
		return true
	}
	return false
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("preset dir %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// BuildPersona 按 Mode 合成出站 system 文本（语义与 Pi-Web 逐条对齐）：
//   - raw     ：只用预设文本；
//   - compose ：网关提示词 + 预设文本；
//   - purge   ：预设文本 + 去掉身份段的网关提示词。
//
// 空预设文本返回空串，调用方据此"什么都不做"（默认不破甲）。
func BuildPersona(base, inject string, mode Mode) string {
	inject = strings.TrimSpace(inject)
	if inject == "" {
		return ""
	}
	base = strings.TrimSpace(base)
	switch mode {
	case ModeRaw:
		return inject
	case ModeCompose:
		if base == "" {
			return inject
		}
		if strings.Contains(base, inject) {
			return base
		}
		return base + "\n\n" + inject
	default: // ModePurge
		rest := StripIdentity(base)
		if rest == "" {
			return inject
		}
		if strings.Contains(rest, inject) {
			return rest
		}
		return inject + "\n\n" + rest
	}
}

// StripIdentity 去掉网关提示词开头的身份陈述段（"You are …" / "你是…"），
// 其余段落逐字保留——"忘了自己是谁"仍要能干活。
func StripIdentity(text string) string {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	markers := []string{"you are ", "you're ", "your name is ", "your working name is ", "你是", "你的名字是", "你的工作名是"}
	hit := false
	for _, m := range markers {
		if strings.HasPrefix(lower, m) || strings.HasPrefix(trimmed, m) {
			hit = true
			break
		}
	}
	if !hit {
		return trimmed
	}
	// 身份段结束于第一个空行；没有空行就切到第一个换行。
	cut := strings.Index(trimmed, "\n\n")
	if cut < 0 {
		if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
			cut = nl
		} else {
			return ""
		}
	}
	return strings.TrimLeft(trimmed[cut:], " \t\r\n")
}

// ZeroWidthInsert 在文本里每 gap 个字符插一个零宽空格（U+200B）。
//
// 用途只有一个：让上游外审的逐字匹配失配（报告 N2 / 3105653#12）。代价是
// 代码与命令不可用，所以调用方必须默认关闭、并按模型显式启用。
// 幂等：文本里已含零宽字符时原样返回，重试不会叠加。
func ZeroWidthInsert(text string, gap int) string {
	if text == "" || strings.ContainsRune(text, '\u200b') {
		return text
	}
	if gap <= 0 {
		gap = ZeroWidthGap
	}
	var b strings.Builder
	b.Grow(len(text) + len(text)/gap + 1)
	count := 0
	for _, r := range text {
		b.WriteRune(r)
		count++
		if count%gap == 0 {
			b.WriteRune('\u200b')
		}
	}
	return b.String()
}

// StripZeroWidth 去掉零宽字符（测试与还原用）。
func StripZeroWidth(text string) string {
	return strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\ufeff", "").Replace(text)
}

// ByteLen 文本字节长度（面板展示用）。
func ByteLen(text string) int { return len(text) }

// RuneLen 文本字符数（变形间隔按字符计）。
func RuneLen(text string) int { return utf8.RuneCountInString(text) }

// ParseImport 把社区格式的预设 JSON 解析成 Preset 列表（PRD v0.2 US-009）。
//
// 支持的字段布局（同名字段冲突时按 agent-purge > keysmith > dsh-breach > pi-web 取）：
//   - 原生：{"id","name","kind","mode","text","tags","source"} 或其数组
//   - agent-purge / purge：{name, prompt, tags}
//   - keysmith：{preset | persona}（字符串或对象里的 name/prompt）
//   - dsh-breach：{name, text}
//   - pi-web：{id, label, hint, mode, text}（mode 原样保留）
//
// 无法识别的字段忽略；缺 name 用 fallbackName 兜底；没有正文则整体报错——
// 静默产出一条空预设比导入失败更难排查。
func ParseImport(raw []byte, fallbackName string) ([]Preset, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty import payload")
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("import json: %w", err)
	}
	var nodes []any
	switch v := doc.(type) {
	case []any:
		nodes = v
	case map[string]any:
		if list, ok := v["presets"].([]any); ok {
			nodes = list
		} else {
			nodes = []any{v}
		}
	default:
		return nil, errors.New("import payload must be an object or an array")
	}
	out := make([]Preset, 0, len(nodes))
	for _, n := range nodes {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		p, ok := presetFromMap(m, fallbackName)
		if !ok {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("no preset with text found in payload")
	}
	return out, nil
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func presetFromMap(m map[string]any, fallbackName string) (Preset, bool) {
	text := str(m, "text", "prompt", "persona", "preset", "body")
	if text == "" {
		return Preset{}, false
	}
	name := str(m, "name", "label", "title", "id")
	id := str(m, "id", "key", "slug")
	if id == "" {
		id = name
	}
	if id == "" {
		id = fallbackName
	}
	if name == "" {
		name = id
	}
	source := Source(str(m, "source"))
	if !source.valid() {
		source = SourceManual
	}
	// 布局指纹：用来在缺 source 时给出来源，面板导入历史要能看出「这是谁家的格式」。
	if _, ok := m["hint"]; ok && source == SourceManual {
		source = SourcePiWeb
	} else if _, ok := m["tags"]; ok && source == SourceManual {
		source = SourceAgentPurge
	}
	if _, ok := m["Behavior"]; ok && source == SourceManual {
		source = SourceDSHBreach
	}
	kind := Kind(str(m, "kind"))
	if !kind.valid() {
		kind = KindPersona
	}
	mode := Mode(strings.ToLower(str(m, "mode")))
	if !mode.valid() {
		mode = ModePurge
	}
	tags := []string{}
	if list, ok := m["tags"].([]any); ok {
		for _, t := range list {
			if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
				tags = append(tags, s)
			}
		}
	}
	return Preset{
		ID: id, Name: name, Kind: kind, Mode: mode, Text: text,
		Tags: tags, Source: source, Enabled: true, UpdatedAt: time.Now(),
	}, true
}

// ExportAgentPurge 以 agent-purge 的字段布局导出（社区迁移量最大的一种）。
func ExportAgentPurge(p Preset) map[string]any {
	out := map[string]any{"name": p.Name, "prompt": p.Text, "mode": string(p.Mode)}
	if len(p.Tags) > 0 {
		out["tags"] = p.Tags
	}
	return out
}