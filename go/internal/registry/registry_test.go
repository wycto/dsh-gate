package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/registry"
)

func newRecord(name string, port int) registry.Record {
	return registry.Record{
		Name:      name,
		PID:       os.Getpid(),
		Exe:       "/usr/local/bin/dsh-gate",
		Argv:      []string{"/usr/local/bin/dsh-gate", "run", "--name", name, "--port", "8080"},
		Listen:    "0.0.0.0:8080",
		Port:      port,
		DSHPort:   3080,
		DSHHome:   "/home/me/.dsh-" + name,
		UsersFile: "/home/me/.dsh-gate/users.json",
		StateDir:  "/home/me/.dsh-gate",
		Version:   "0.2.0",
		StartedAt: time.Now(),
	}
}

func TestSaveLoadRemove(t *testing.T) {
	dir := registry.Dir(t.TempDir())

	record := newRecord("personal", 8080)
	if err := dir.Save(record); err != nil {
		t.Fatalf("写入登记失败: %v", err)
	}

	loaded, ok, err := dir.Load("personal")
	if err != nil || !ok {
		t.Fatalf("读取登记失败: ok=%v err=%v", ok, err)
	}
	// 往返之后命令行必须原样还在——restart 全靠它。
	if len(loaded.Argv) != len(record.Argv) {
		t.Fatalf("命令行丢失: %q", loaded.Argv)
	}
	for i := range loaded.Argv {
		if loaded.Argv[i] != record.Argv[i] {
			t.Fatalf("命令行第 %d 项 = %q，期望 %q", i, loaded.Argv[i], record.Argv[i])
		}
	}
	if loaded.DSHHome != record.DSHHome || loaded.Port != record.Port {
		t.Errorf("字段没有往返: %+v", loaded)
	}

	if err := dir.Remove("personal"); err != nil {
		t.Fatalf("删除登记失败: %v", err)
	}
	if _, ok, _ := dir.Load("personal"); ok {
		t.Error("删除后不应还能读到")
	}
	// 删一个不存在的登记不算错误：stop 是幂等的。
	if err := dir.Remove("personal"); err != nil {
		t.Errorf("重复删除不应报错: %v", err)
	}
}

func TestListSortsAndIgnoresNonJSON(t *testing.T) {
	dir := registry.Dir(t.TempDir())
	for _, name := range []string{"work", "personal", "default"} {
		if err := dir.Save(newRecord(name, 8080)); err != nil {
			t.Fatal(err)
		}
	}
	// 混进去一个不该被当成登记的文件。
	if err := os.WriteFile(filepath.Join(string(dir), "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	records, problems := dir.List()
	if len(problems) != 0 {
		t.Fatalf("不该有解析问题: %v", problems)
	}
	if len(records) != 3 {
		t.Fatalf("应当有 3 条，实际 %d", len(records))
	}
	if records[0].Name != "default" || records[2].Name != "work" {
		t.Errorf("没有按名字排序: %s, %s, %s", records[0].Name, records[1].Name, records[2].Name)
	}
}

func TestListSurvivesCorruptFile(t *testing.T) {
	dir := registry.Dir(t.TempDir())
	if err := dir.Save(newRecord("good", 8080)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(string(dir), "bad.json"), []byte("{ 坏文件"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 一个文件坏掉不该让整条 status 失败：其余实例的状态仍然有价值。
	records, problems := dir.List()
	if len(records) != 1 || records[0].Name != "good" {
		t.Errorf("坏文件不应影响其余登记，实际读到 %d 条", len(records))
	}
	if len(problems) != 1 {
		t.Errorf("应当报告一个问题，实际 %d 个", len(problems))
	}
}

func TestFind(t *testing.T) {
	dir := registry.Dir(t.TempDir())
	for _, name := range []string{"work", "personal"} {
		if err := dir.Save(newRecord(name, 8080)); err != nil {
			t.Fatal(err)
		}
	}

	all, err := dir.Find(nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("不写名字应当返回全部: %v %d", err, len(all))
	}

	one, err := dir.Find([]string{"work"})
	if err != nil || len(one) != 1 || one[0].Name != "work" {
		t.Fatalf("按名字取失败: %v %v", err, one)
	}

	// 点名了却不存在必须报错：静默跳过会让人以为「停了两个」其实只停了一个。
	if _, err := dir.Find([]string{"work", "missing"}); err == nil {
		t.Fatal("点名了不存在的实例应当报错")
	}
}

func TestListOnMissingDir(t *testing.T) {
	// 一台没跑过网关的机器上，登记目录根本不存在。
	records, problems := registry.Dir(filepath.Join(t.TempDir(), "nope")).List()
	if len(records) != 0 || len(problems) != 0 {
		t.Errorf("目录不存在应当返回空: %v %v", records, problems)
	}
}

func TestPathSanitizesInstanceName(t *testing.T) {
	// 登记文件是自动写入的，实例名一旦能带路径就成了往任意位置写文件的口子。
	dir := registry.Dir(t.TempDir())
	// 名字里的斜杠与点全部被替换成下划线，落点仍在登记目录内——这才是要保证的。
	hostile := dir.Path("../../etc/passwd")
	if filepath.Dir(hostile) != string(dir) {
		t.Errorf("路径逃出了登记目录: %s", hostile)
	}
	if strings.ContainsAny(filepath.Base(hostile), "/\\") {
		t.Errorf("文件名里仍有路径分隔符: %s", hostile)
	}
	if got := dir.Path("normal-1_2"); got != filepath.Join(string(dir), "normal-1_2.json") {
		t.Errorf("正常名字被改动了: %s", got)
	}
}

func TestTildeRunDirExpands(t *testing.T) {
	// 默认登记目录是 `~/.dsh-gate/run`。Windows 上 filepath.Join 产出的是 `~\...`，
	// 而 expandPath 曾经只认 `~/`，于是登记写进「当前目录下的 ~ 文件夹」，换个目录
	// 再 status 就找不到刚起的实例（真机 e2e 里暴露出来的）。
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := registry.Dir(filepath.Join("~/.dsh-gate", "run"))
	expanded := string(dir.Expand())
	if strings.HasPrefix(expanded, "~") {
		t.Fatalf("默认登记目录没有展开: %q", expanded)
	}
	if !strings.HasPrefix(expanded, home) {
		t.Fatalf("应展开到家目录 %q，实际 %q", home, expanded)
	}

	// 反斜杠写法（Windows 上 filepath.Join 的真实产物）同样要展开。
	backslash := registry.Dir(`~\.dsh-gate\run`)
	if got := string(backslash.Expand()); strings.HasPrefix(got, "~") {
		t.Fatalf("反斜杠形式的默认目录没有展开: %q", got)
	}

	// Save / Load / List 必须口径一致：Path 也要走展开，否则 rename 的目标会指向
	// 一个字面量 `~` 目录，写完读不回来。
	if err := dir.Save(newRecord("expanded", 8080)); err != nil {
		t.Fatalf("写入登记失败: %v", err)
	}
	record, ok, err := dir.Load("expanded")
	if err != nil || !ok {
		t.Fatalf("写进去的登记读不回来: ok=%v err=%v", ok, err)
	}
	if record.Name != "expanded" {
		t.Fatalf("记录内容不对: %+v", record)
	}
	if _, err := os.Stat(filepath.Join(home, ".dsh-gate", "run", "expanded.json")); err != nil {
		t.Fatalf("登记应落在展开后的家目录里: %v", err)
	}
}

func TestAlive(t *testing.T) {
	// 存活判定看端口，不看进程号：进程号会被系统回收复用，端口不会。
	closed := newRecord("gone", 1) // 端口 1 几乎不可能有人监听
	if closed.Alive() {
		t.Error("没人监听的端口不应判定为存活")
	}
}
