package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
