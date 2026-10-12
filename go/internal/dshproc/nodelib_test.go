package dshproc

import (
	"errors"
	"strings"
	"testing"
)

// TestMissingSharedLibrary 从 loader 的原话里认出缺哪个库——认错了就会去装一个
// 不相干的包，那比不装更糟。
func TestMissingSharedLibrary(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "缺 libatomic（实测踩到的那个）",
			err: errors.New("自备的 Node.js 运行时无法执行（exit status 127）：" +
				"node: error while loading shared libraries: libatomic.so.1: cannot open shared object file: No such file or directory"),
			want: "libatomic.so.1",
		},
		{
			name: "缺 libstdc++",
			err:  errors.New("node: error while loading shared libraries: libstdc++.so.6: cannot open shared object file"),
			want: "libstdc++.so.6",
		},
		{name: "缺库以外的失败不认", err: errors.New("exit status 1: SyntaxError"), want: ""},
		{name: "没有错误", err: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := missingSharedLibrary(tc.err); got != tc.want {
				t.Errorf("missingSharedLibrary = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestLibraryPackagesAreConcrete 每个库都要映射到一个具体的包名，否则补装那一步
// 会拿着一个空包名去调包管理器。
func TestLibraryPackagesAreConcrete(t *testing.T) {
	if len(libraryPackages) == 0 {
		t.Fatal("映射表是空的")
	}
	for library, pkg := range libraryPackages {
		if !strings.HasSuffix(library, ".so.1") && !strings.Contains(library, ".so.") {
			t.Errorf("库名 %q 看起来不像共享库", library)
		}
		if strings.TrimSpace(pkg) == "" || strings.ContainsAny(pkg, " \t") {
			t.Errorf("库 %s 的包名不合法: %q", library, pkg)
		}
	}
	if libraryPackages["libatomic.so.1"] != "libatomic1" {
		t.Error("libatomic.so.1 必须映射到 libatomic1（实测用的是这个包）")
	}
}
