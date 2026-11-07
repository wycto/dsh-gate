package portal

import (
	"path/filepath"
	"testing"
)

func TestSiteTitleRoundtrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer s.Close()

	if v, err := s.SiteTitle(); err != nil || v != "" {
		t.Fatalf("默认应为空: %q / %v", v, err)
	}
	if err := s.SetSiteTitle("  office-gw  "); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	if v, _ := s.SiteTitle(); v != "office-gw" {
		t.Errorf("应去空白后为 office-gw，实际 %q", v)
	}
	if err := s.SetSiteTitle(""); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if v, _ := s.SiteTitle(); v != "" {
		t.Errorf("清空后应为空，实际 %q", v)
	}
	// 超长（>64 字符）应拒绝
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if err := s.SetSiteTitle(long); err == nil {
		t.Error("超长应被拒绝")
	}
	// 控制字符应拒绝
	if err := s.SetSiteTitle("a\nb"); err == nil {
		t.Error("含换行应被拒绝")
	}
}
