package users_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/users"
)

func TestHashAndVerify(t *testing.T) {
	hashed, err := users.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	if !users.VerifyPassword(hashed, "correct-horse-battery") {
		t.Error("正确口令应当校验通过")
	}
	if users.VerifyPassword(hashed, "correct-horse-batterY") {
		t.Error("错误口令不得通过")
	}
	if users.VerifyPassword(hashed, "") {
		t.Error("空口令不得通过")
	}

	// 两次哈希必须不同（每次新盐），否则相同口令会得到相同哈希串，
	// 攻击者一眼就能看出两个账号用了同一个口令。
	again, _ := users.HashPassword("correct-horse-battery")
	if again == hashed {
		t.Error("相同口令的两次哈希不应相同（缺少随机盐）")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	// 哈希串来自磁盘，格式异常时必须拒绝而不是 panic。
	for _, bad := range []string{
		"", "x", "pbkdf2-sha256$abc$aa$bb", "pbkdf2-sha256$600000$!!$bb",
		"pbkdf2-sha256$1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcA",
		"md5$1$aa$bb", "pbkdf2-sha256$600000$YWJj$!!",
	} {
		if users.VerifyPassword(bad, "whatever") {
			t.Errorf("畸形哈希 %q 不应通过校验", bad)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store, err := users.Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if store.Count() != 0 {
		t.Fatalf("新账号表应当为空，实际 %d 个", store.Count())
	}
	// 空账号表上的登录必须是 false，且不能报错。
	if store.Verify("anyone", "anything") {
		t.Error("空账号表不应放行任何口令")
	}

	if err := store.Set("alice", "password-1234", 50_000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := store.Set("bob", "password-5678", 50_000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if store.Count() != 2 {
		t.Fatalf("应当有 2 个账号，实际 %d", store.Count())
	}
	if !store.Exists("alice") || store.Exists("carol") {
		t.Error("Exists 判断不对")
	}
	if record, ok := store.Get("alice"); !ok || record.CreatedAt.IsZero() {
		t.Error("Get 没有返回创建时间")
	}

	// 重新打开后仍可校验，证明确实落盘了。
	reopened, err := users.Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if !reopened.Verify("alice", "password-1234") {
		t.Error("重开账号表后校验失败")
	}
	if reopened.Verify("alice", "password-1235") {
		t.Error("重开账号表后错误口令竟然通过")
	}
	if names := reopened.Names(); len(names) != 2 || names[0] != "alice" || names[1] != "bob" {
		t.Errorf("Names = %v", names)
	}
}

func TestStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")
	store, err := users.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("alice", "password-1234", 50_000); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 账号表等价于万能钥匙，同机其它用户不该读得到。
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("账号表权限 = %o，期望 600", perm)
	}
}

func TestStoreRefusesToRemoveLastAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store, _ := users.Open(path)
	if err := store.Set("only", "password-1234", 50_000); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("only"); err == nil {
		t.Error("拒绝删除最后一个账号")
	}
	if err := store.Remove("nobody"); err == nil {
		t.Error("删除不存在的账号应当报错")
	}
}

func TestStoreRejectsInvalidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	store, _ := users.Open(path)

	if err := store.Set("", "password-1234", 0); err == nil {
		t.Error("空用户名应当被拒绝")
	}
	if err := store.Set("has space", "password-1234", 0); err == nil {
		t.Error("含空格的用户名应当被拒绝")
	}
	if err := store.Set("alice", "short", 0); err == nil {
		t.Error("过短的口令应当被拒绝")
	}
	if err := store.Set("alice", strings.Repeat("x", 300), 0); err == nil {
		t.Error("过长的口令应当被拒绝")
	}
	if err := store.Set("alice", "password-1234", 10); err == nil {
		t.Error("过低的迭代次数应当被拒绝")
	}
	if err := store.Set("alice", "password-1234", 1_000_000_000); err == nil {
		t.Error("过高的迭代次数应当被拒绝")
	}
}

func TestValidatePasswordCountsRunes(t *testing.T) {
	// 中文口令按字符数算长度，否则一个 4 字口令会被 UTF-8 字节数误判为「太长」，
	// 而实际上它完全够用。
	if err := users.ValidatePassword("这个中文口令足够长了"); err != nil {
		t.Errorf("9 个字符的中文口令应当通过，实际: %v", err)
	}
	if err := users.ValidatePassword("太短"); err == nil {
		t.Error("两个字符的口令应当被拒绝")
	}
}

func TestOpenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 损坏的账号表必须报错而不是静默重建：那等于把所有人锁在门外且毫无线索。
	if _, err := users.Open(path); err == nil {
		t.Fatal("损坏的账号表应当报错")
	}
}

// TestStorePicksUpExternalChanges 锁定「改密不必重启网关」。
//
// 网关进程只在启动时 Open 一次账号表，而 user add / passwd / rm 是另一个
// 进程在改文件。如果 Verify 不重读，就会出现「明明改了密码还是登不上」，
// 而且没有报错指引人去重启。
func TestStorePicksUpExternalChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")

	gateway, err := users.Open(path) // 网关进程持有的那份
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	admin, err := users.Open(path) // user add / passwd 进程的那份
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}

	// 另一个进程创建账号：网关那份在下一次校验时应当能看到。
	if err := admin.Set("admin", "旧口令-abcd1234", users.DefaultIterations); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	if !gateway.Verify("admin", "旧口令-abcd1234") {
		t.Fatal("外部进程创建的账号，网关进程应能立即校验通过")
	}

	// 另一个进程改密：旧口令立刻失效，新口令立刻生效。
	if err := admin.Set("admin", "新口令-abcd1234", users.DefaultIterations); err != nil {
		t.Fatalf("修改口令失败: %v", err)
	}
	if gateway.Verify("admin", "旧口令-abcd1234") {
		t.Error("外部改密后旧口令仍然有效")
	}
	if !gateway.Verify("admin", "新口令-abcd1234") {
		t.Error("外部改密后新口令没有生效")
	}

	// 另一个进程删除账号（账号表拒绝删掉最后一个，先垫一个进去）。
	if err := admin.Set("second", "垫底口令-abcd1", users.DefaultIterations); err != nil {
		t.Fatalf("准备第二个账号失败: %v", err)
	}
	if err := admin.Remove("admin"); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}
	if gateway.Verify("admin", "新口令-abcd1234") {
		t.Error("外部删除账号后仍然能登录")
	}
}
