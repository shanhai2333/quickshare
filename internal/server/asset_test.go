package server

import (
	"testing"
	"testing/fstest"
)

// 资源版本号是"升级后不拿旧 JS/CSS 配新服务端"的唯一保障，
// 而 e2e 只能验证页面里带了版本号，验证不了它是否真的跟着内容变。
// 这里把两条契约钉死：内容变则版本变，同一份内容则版本稳定。
func TestAssetVersion(t *testing.T) {
	old := fstest.MapFS{
		"style.css": &fstest.MapFile{Data: []byte("a{color:red}")},
		"app.js":    &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	newer := fstest.MapFS{
		"style.css": &fstest.MapFile{Data: []byte("a{color:blue}")},
		"app.js":    &fstest.MapFile{Data: []byte("console.log(1)")},
	}

	if got, want := assetVersion(old), assetVersion(old); got != want {
		t.Fatalf("同一份资源两次算出的版本号不一致：%s vs %s", got, want)
	}
	if assetVersion(old) == assetVersion(newer) {
		t.Fatal("资源内容变了，版本号却没变——浏览器会继续用旧缓存")
	}
	if n := len(assetVersion(old)); n != 16 {
		t.Fatalf("版本号应为 8 字节十六进制（16 字符），实际 %d 字符", n)
	}
}
