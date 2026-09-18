package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 老库（没有 ttl_seconds / is_code 那几列）必须能被 Open 自动补上。
//
// 这条钉的是最容易漏的一环：`schema` 全是 `CREATE TABLE IF NOT EXISTS`，对
// **已经存在**的表什么都不做，新加的列在老库上永远补不上来——而报错要等到第一次
// 用到那列时才出现（`no such column: ttl_seconds`），看起来像代码写错了。
// 所以这里手工造一个老结构的库，再走正常流程打开它。
func TestMigrateAddsTTLColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	oldSchema := `
CREATE TABLE files (
  id           TEXT    PRIMARY KEY,
  name         TEXT    NOT NULL,
  size         INTEGER NOT NULL,
  mime         TEXT    NOT NULL DEFAULT '',
  status       TEXT    NOT NULL DEFAULT 'uploading',
  fingerprint  TEXT    NOT NULL DEFAULT '',
  chunk_size   INTEGER NOT NULL DEFAULT 0,
  total_chunks INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  completed_at INTEGER
);
CREATE TABLE texts (
  id         TEXT    PRIMARY KEY,
  content    TEXT    NOT NULL,
  device_id  TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);`

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("建老表失败: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO texts (id, content, device_id, created_at, updated_at)
		 VALUES ('t1', '老数据', 'dev', 1, 1)`); err != nil {
		t.Fatalf("插入老数据失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级老库失败: %v", err)
	}
	defer st.Close()

	// 老数据还在，新列拿到默认值
	got, err := st.GetText("t1")
	if err != nil {
		t.Fatalf("读老数据失败: %v", err)
	}
	if got.Content != "老数据" {
		t.Fatalf("老数据内容 = %q，期望 %q", got.Content, "老数据")
	}
	if got.TTLSeconds != 0 || got.IsCode {
		t.Fatalf("补上来的新列应当是默认值，实际 %+v", got)
	}

	// 文本表的新列能写
	if err := st.SetTextTTL("t1", 600); err != nil {
		t.Fatalf("写文本表的新列失败: %v", err)
	}

	// 文件表的新列也能写（正向验证：真的建了列，而不是"报了个别的错"）
	if err := st.CreateUpload(&File{
		ID: "f1", Name: "a.txt", Size: 1, CreatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("插入文件失败: %v", err)
	}
	if err := st.SetFileTTL("f1", 600); err != nil {
		t.Fatalf("写文件表的新列失败: %v", err)
	}
	f, err := st.GetFile("f1")
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	if f.TTLSeconds != 600 {
		t.Fatalf("文件保留时长 = %d，期望 600", f.TTLSeconds)
	}
}

// 重复打开同一个库不能因为"列已经存在"而报错。
//
// ALTER TABLE 没有 `IF NOT EXISTS`，所以迁移必须自己先查 PRAGMA —— 漏了的话
// 第二次启动就会 duplicate column name，而这个错只在"第二次启动"时才出现，
// 单次启动的测试根本发现不了。
func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	for i := 1; i <= 3; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次打开失败: %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("第 %d 次关库失败: %v", i, err)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"普通文件名", "report.pdf", "report.pdf"},
		{"中文文件名", "部署说明.txt", "部署说明.txt"},
		{"路径穿越被剥掉目录", "../../../etc/passwd", "passwd"},
		{"反斜杠也当分隔符", `..\..\windows\system32\cmd.exe`, "cmd.exe"},
		{"非法字符替换为下划线", `a:b*c?d"e<f>g|h`, "a_b_c_d_e_f_g_h"},
		{"控制字符被删除", "a\x00b\x1fc\x7fd", "abcd"},
		{"首尾点与空格被去掉", "  ..name..  ", "name"},
		{"全是点则回落到 unnamed", "...", "unnamed"},
		{"空字符串回落到 unnamed", "", "unnamed"},
		{"恰好 200 字节不截断", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"超长 ASCII 截到 200", strings.Repeat("a", 250), strings.Repeat("a", 200)},
		{"多字节字符正好放得下", strings.Repeat("a", 197) + "中", strings.Repeat("a", 197) + "中"},
		// 这条钉的是一个真 bug：按字节切 name[:200] 会把第 200 字节处的汉字
		// 劈成半个，落库是非法 UTF-8，页面显示成一个 � 方块。
		// 现在按字符边界截断——宁可少放几个字节，也不切碎字符。
		{"多字节字符不被切碎", strings.Repeat("a", 199) + "中", strings.Repeat("a", 199)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeName(tt.in)
			if got != tt.want {
				t.Errorf("SanitizeName(%q) = %q，期望 %q", tt.in, got, tt.want)
			}
			// 不管输入多刁钻，结果都必须是合法 UTF-8 且不超上限
			if !utf8.ValidString(got) {
				t.Errorf("SanitizeName(%q) 返回了非法 UTF-8: %q", tt.in, got)
			}
			if len(got) > maxNameBytes {
				t.Errorf("SanitizeName(%q) 长度 %d 字节，超过上限 %d", tt.in, len(got), maxNameBytes)
			}
		})
	}
}
