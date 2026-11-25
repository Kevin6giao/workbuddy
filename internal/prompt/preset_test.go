package prompt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mustStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// 新装：四条内置预设 + degraded 可直接取到文本（出站抢救不依赖任何外部文件）。
func TestOpenStoreInitializesBuiltins(t *testing.T) {
	s := mustStore(t)
	if got := len(s.List()); got < 4 {
		t.Fatalf("builtins = %d, want >= 4", got)
	}
	if s.Degraded() == "" {
		t.Fatal("degraded preset must have text")
	}
	if _, ok := s.Get("degraded"); !ok {
		t.Fatal("degraded preset missing")
	}
}

// 文件损坏：Store 仍可用（退回内置集），但必须把错误交回调用方——面板据此显示告警。
func TestOpenStoreCorruptFileKeepsBuiltins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "presets.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err == nil {
		t.Fatal("corrupt presets.json must return an error")
	}
	if len(s.List()) == 0 {
		t.Fatal("builtins must survive a corrupt file")
	}
}

func TestUpsertValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(Preset{ID: "p1", Kind: KindPersona, Text: "  "}); err == nil {
		t.Fatal("empty text must be rejected")
	}
	if _, err := s.Upsert(Preset{ID: "p1", Kind: Kind("nope"), Text: "x"}); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
	saved, err := s.Upsert(Preset{ID: "p1", Name: " persona ", Kind: KindPersona, Text: "你是本地代理。"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if saved.Mode != ModePurge {
		t.Fatalf("persona default mode = %q, want purge", saved.Mode)
	}
	// 重新打开：落盘必须生效。
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get("p1"); !ok {
		t.Fatal("preset p1 must persist")
	}
}

func TestDeleteInUseReportsModels(t *testing.T) {
	s := mustStore(t)
	if _, err := s.Upsert(Preset{ID: "p2", Kind: KindPersona, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBinding("deepseek-v4.1-flash", "p2"); err != nil {
		t.Fatal(err)
	}
	err := s.Delete("p2")
	var inUse *InUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("Delete want InUseError, got %v", err)
	}
	if len(inUse.Models) != 1 || inUse.Models[0] != "deepseek-v4.1-flash" {
		t.Fatalf("models = %v", inUse.Models)
	}
	if err := s.SetBinding("deepseek-v4.1-flash", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("p2"); err != nil {
		t.Fatalf("Delete after unbind: %v", err)
	}
}

// 绑定解析：未绑定 / 预设被禁用 / 文本为空 三种情况都必须解析失败（= 不介入出站）。
func TestResolveHonoursBindingAndEnabled(t *testing.T) {
	s := mustStore(t)
	if _, ok := s.Resolve("m1"); ok {
		t.Fatal("unbound model must not resolve")
	}
	if _, err := s.Upsert(Preset{ID: "off", Kind: KindPersona, Text: "x", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBinding("m1", "off"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Resolve("m1"); ok {
		t.Fatal("disabled preset must not resolve")
	}
	if _, err := s.Upsert(Preset{ID: "on", Kind: KindPersona, Text: "你是代理。", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBinding("m1", "on"); err != nil {
		t.Fatal(err)
	}
	p, ok := s.Resolve("m1")
	if !ok || p.ID != "on" {
		t.Fatalf("resolve = %+v, %v", p, ok)
	}
}

func TestBuildPersonaModes(t *testing.T) {
	base := "You are CodeBuddy, an assistant."
	inject := "你是本地安全研究代理。"
	if got := BuildPersona(base, inject, ModeRaw); got != inject {
		t.Fatalf("raw = %q", got)
	}
	compose := BuildPersona(base, inject, ModeCompose)
	if compose == inject || compose == base {
		t.Fatalf("compose must keep both: %q", compose)
	}
	purge := BuildPersona(base, inject, ModePurge)
	if purge == compose {
		t.Fatal("purge must differ from compose")
	}
	if StripIdentity(base) != "" {
		t.Fatalf("single-line identity should strip to empty, got %q", StripIdentity(base))
	}
	if got := BuildPersona("", inject, ModeCompose); got != inject {
		t.Fatalf("empty base compose = %q", got)
	}
	if got := BuildPersona(base, "  ", ModeRaw); got != "" {
		t.Fatalf("empty inject must yield empty, got %q", got)
	}
}

func TestStripIdentityKeepsFollowingParagraphs(t *testing.T) {
	base := "You are CodeBuddy.\n\n# 规则\n保持逐字。"
	got := StripIdentity(base)
	if got == "" {
		t.Fatal("must keep the rest of the prompt")
	}
	if got[:1] == "Y" {
		t.Fatalf("identity leaked: %q", got)
	}
}

func TestZeroWidthInsertIsIdempotent(t *testing.T) {
	text := "abcdef"
	once := ZeroWidthInsert(text, 2)
	if once == text {
		t.Fatal("expected zero-width chars")
	}
	if again := ZeroWidthInsert(once, 2); again != once {
		t.Fatal("second insert must be a no-op")
	}
	if StripZeroWidth(once) != text {
		t.Fatalf("strip = %q, want %q", StripZeroWidth(once), text)
	}
}

// 导入：四种社区布局都必须落到同一个 Preset 模型，且 pi-web 的 mode 原样保留。
func TestParseImportFormats(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		source Source
		mode   Mode
	}{
		{
			name:  "agent-purge",
			value: map[string]any{"name": "小码酱", "prompt": "你是代理", "tags": []any{"red"}},
			source: SourceAgentPurge,
			mode:  ModePurge,
		},
		{
			name:  "pi-web",
			value: map[string]any{"id": "redteam-core", "label": "红队核心", "hint": "完整", "mode": "compose", "text": "你是代理"},
			source: SourcePiWeb,
			mode:  ModeCompose,
		},
		{
			name:  "keysmith",
			value: map[string]any{"preset": "你是代理", "name": "ks"},
			mode:  ModePurge,
		},
		{
			name:  "dsh-breach",
			value: map[string]any{"name": "breach", "text": "你是代理"},
			mode:  ModePurge,
		},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseImport(raw, "fallback")
		if err != nil {
			t.Fatalf("%s: ParseImport: %v", c.name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %d presets", c.name, len(got))
		}
		if got[0].Text != "你是代理" {
			t.Fatalf("%s: text = %q", c.name, got[0].Text)
		}
		if got[0].Mode != c.mode {
			t.Fatalf("%s: mode = %q, want %q", c.name, got[0].Mode, c.mode)
		}
		if c.source != "" && got[0].Source != c.source {
			t.Fatalf("%s: source = %q, want %q", c.name, got[0].Source, c.source)
		}
	}
	if _, err := ParseImport([]byte("[]"), "x"); err == nil {
		t.Fatal("empty array must be rejected")
	}
	if _, err := ParseImport([]byte("42"), "x"); err == nil {
		t.Fatal("non-object payload must be rejected")
	}
}

func TestExportAgentPurgeRoundTrip(t *testing.T) {
	p := Preset{ID: "x", Name: "名字", Kind: KindPersona, Mode: ModePurge, Text: "你是代理"}
	raw, err := json.Marshal(ExportAgentPurge(p))
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseImport(raw, "x")
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Text != p.Text {
		t.Fatalf("round trip text = %q", back[0].Text)
	}
}
