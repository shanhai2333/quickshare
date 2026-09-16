package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore 开一个挂在临时目录上的库。用完自动清理。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mkText(id, content, device string) *Text {
	now := time.Unix(time.Now().Unix(), 0) // 截到秒，和真实调用一致
	return &Text{ID: id, Content: content, DeviceID: device, CreatedAt: now, UpdatedAt: now}
}

func TestTextCRUD(t *testing.T) {
	st := newTestStore(t)

	if err := st.CreateText(mkText("t1", "第一条", "dev-a")); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	got, err := st.GetText("t1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Content != "第一条" || got.DeviceID != "dev-a" {
		t.Fatalf("读回的内容不对: %+v", got)
	}

	if err := st.UpdateText("t1", "改过了"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got, _ = st.GetText("t1")
	if got.Content != "改过了" {
		t.Fatalf("更新后内容 = %q，期望 %q", got.Content, "改过了")
	}

	if err := st.DeleteText("t1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := st.GetText("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后读取应当返回 ErrNotFound，实际 %v", err)
	}
}

// 改 / 删一个不存在的 ID 必须报 ErrNotFound。
//
// 这条钉的是"UPDATE 影响 0 行不报错"这个 SQL 语义：不自己查 RowsAffected
// 的话，接口会对着不存在的记录回 200，前端以为改成功了。
func TestUpdateDeleteMissingText(t *testing.T) {
	st := newTestStore(t)

	if err := st.UpdateText("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改不存在的记录应当返回 ErrNotFound，实际 %v", err)
	}
	if err := st.DeleteText("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的记录应当返回 ErrNotFound，实际 %v", err)
	}
}

// 同一秒内连发的几条必须顺序稳定。
//
// created_at 只到秒，只按它排序时 SQLite 对并列行不作保证，列表会"跳"。
// 和文件列表是同一个坑，所以按 rowid 兜底（rowid 越大越新）。
func TestListTextsOrderStableWithinSameSecond(t *testing.T) {
	st := newTestStore(t)

	// 故意用同一个时间戳，模拟"一秒内连发三条"
	same := time.Unix(1700000000, 0)
	for _, id := range []string{"a", "b", "c"} {
		tt := &Text{ID: id, Content: id, CreatedAt: same, UpdatedAt: same}
		if err := st.CreateText(tt); err != nil {
			t.Fatalf("插入 %s 失败: %v", id, err)
		}
	}

	want := []string{"c", "b", "a"} // 最新的在最前
	for i := 0; i < 3; i++ {        // 连查几次，确认不是偶然
		list, err := st.ListTexts()
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("条数 = %d，期望 3", len(list))
		}
		for j, tt := range list {
			if tt.ID != want[j] {
				t.Fatalf("第 %d 次查询顺序 = %v，期望 %v", i, ids(list), want)
			}
		}
	}
}

func ids(list []*Text) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.ID)
	}
	return out
}

// 记录设备时**不能覆盖用户改过的备注**。
//
// 每次请求都会 TouchDevice，如果 upsert 里顺手把 remark 写成空串，
// 用户改的名字下一次发文本就没了。
func TestTouchDeviceKeepsRemark(t *testing.T) {
	st := newTestStore(t)

	if err := st.TouchDevice("d1", "iPhone UA"); err != nil {
		t.Fatalf("首次记录失败: %v", err)
	}
	if err := st.SetDeviceRemark("d1", "我的手机"); err != nil {
		t.Fatalf("改备注失败: %v", err)
	}

	// 再来一次（换了 UA，模拟浏览器升级）
	if err := st.TouchDevice("d1", "iPhone UA v2"); err != nil {
		t.Fatalf("再次记录失败: %v", err)
	}

	list, err := st.ListDevices()
	if err != nil {
		t.Fatalf("设备列表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("设备数 = %d，期望 1（upsert 不该插出第二条）", len(list))
	}
	if list[0].Remark != "我的手机" {
		t.Fatalf("备注 = %q，期望被保留为 %q", list[0].Remark, "我的手机")
	}
	if list[0].UA != "iPhone UA v2" {
		t.Fatalf("UA = %q，期望更新为 %q", list[0].UA, "iPhone UA v2")
	}
}

// 把备注设成和原来一样的值，也必须算成功。
//
// SQLite 的 RowsAffected 数的是 WHERE 命中的行数（不是"值真的变了"的行数），
// 所以重复设同一个值仍然返回 1。这条把它钉住——万一哪天换库或改写法导致
// 变成 0，接口会莫名其妙地对一个存在的设备回 404。
func TestSetDeviceRemarkIdempotent(t *testing.T) {
	st := newTestStore(t)
	if err := st.TouchDevice("d1", "ua"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := st.SetDeviceRemark("d1", "同名"); err != nil {
			t.Fatalf("第 %d 次设置同一个备注失败: %v", i+1, err)
		}
	}
	if err := st.SetDeviceRemark("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改不存在的设备应当返回 ErrNotFound，实际 %v", err)
	}
}

// 备注可以清空（恢复成用 UA 解析出来的名字）。
func TestSetDeviceRemarkCanClear(t *testing.T) {
	st := newTestStore(t)
	if err := st.TouchDevice("d1", "ua"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if err := st.SetDeviceRemark("d1", "名字"); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	if err := st.SetDeviceRemark("d1", ""); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	list, _ := st.ListDevices()
	if len(list) != 1 || list[0].Remark != "" {
		t.Fatalf("清空后备注 = %q，期望空串", list[0].Remark)
	}
}

// ListDevices 要带上每台设备还剩几条文本。
//
// 界面上"还有 N 条文本 / 没有文本"和删设备前的确认框都靠它——数错了用户会
// 以为删掉设备不会影响任何东西，结果那台设备的文本全变成「未知设备」。
func TestListDevicesTextCount(t *testing.T) {
	st := newTestStore(t)
	if err := st.TouchDevice("d1", "ua1"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if err := st.TouchDevice("d2", "ua2"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		if err := st.CreateText(mkText(id, "内容", "d1")); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	got := map[string]int{}
	list, err := st.ListDevices()
	if err != nil {
		t.Fatalf("列设备失败: %v", err)
	}
	for _, d := range list {
		got[d.ID] = d.TextCount
	}
	if got["d1"] != 3 || got["d2"] != 0 {
		t.Fatalf("条数不对: %v，期望 d1=3 d2=0", got)
	}

	// 删掉两条之后要跟着变
	if _, err := st.DeleteTexts([]string{"a", "b"}); err != nil {
		t.Fatalf("批量删除失败: %v", err)
	}
	list, _ = st.ListDevices()
	for _, d := range list {
		if d.ID == "d1" && d.TextCount != 1 {
			t.Fatalf("删完两条后 d1 的条数 = %d，期望 1", d.TextCount)
		}
	}
}

// 删设备只删记录，**不碰它的文本**。
func TestDeleteDeviceKeepsTexts(t *testing.T) {
	st := newTestStore(t)
	if err := st.TouchDevice("d1", "ua"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if err := st.SetDeviceRemark("d1", "老王的手机"); err != nil {
		t.Fatalf("设备注失败: %v", err)
	}
	if err := st.CreateText(mkText("t1", "还在", "d1")); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	if err := st.DeleteDevice("d1"); err != nil {
		t.Fatalf("删设备失败: %v", err)
	}

	list, _ := st.ListDevices()
	if len(list) != 0 {
		t.Fatalf("设备应当被删掉，实际还剩 %d 台", len(list))
	}
	// 文本必须还在——用户点的是"把这个设备条目去掉"，不是"清空它的内容"
	txt, err := st.GetText("t1")
	if err != nil {
		t.Fatalf("文本不该被删掉: %v", err)
	}
	if txt.DeviceID != "d1" {
		t.Fatalf("文本的 device_id 不该被动: %q", txt.DeviceID)
	}
}

// 删不存在的设备要报 ErrNotFound，不能让接口对着不存在的记录回 200。
func TestDeleteDeviceNotFound(t *testing.T) {
	st := newTestStore(t)
	if err := st.DeleteDevice("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的设备应当返回 ErrNotFound，实际 %v", err)
	}
}

// ---------------------------------------------------------------- 设备清理

// PruneOrphanDevices 只删"一条文本都不剩"的设备。
//
// 这是「设备随消息删除」那个设置的实现，判据只有一个：还有文本就不能删。
func TestPruneOrphanDevices(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"busy", "empty", "mid"} {
		if err := st.TouchDevice(id, "ua-"+id); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}
	if err := st.SetDeviceRemark("empty", "空设备"); err != nil {
		t.Fatalf("设备注失败: %v", err)
	}
	for _, c := range []struct{ id, dev string }{
		{"t1", "busy"}, {"t2", "busy"}, {"t3", "mid"},
	} {
		if err := st.CreateText(mkText(c.id, "内容", c.dev)); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	n, err := st.PruneOrphanDevices()
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应当只清掉 1 台（empty），实际 %d 台", n)
	}

	left := map[string]bool{}
	list, _ := st.ListDevices()
	for _, d := range list {
		left[d.ID] = true
	}
	if !left["busy"] || !left["mid"] || left["empty"] {
		t.Fatalf("剩下的设备不对: %v", left)
	}

	// 把 mid 的文本删掉，它就该被清掉
	if err := st.DeleteText("t3"); err != nil {
		t.Fatalf("删文本失败: %v", err)
	}
	n, err = st.PruneOrphanDevices()
	if err != nil {
		t.Fatalf("第二次清理失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("第二次应当清掉 mid，实际 %d 台", n)
	}

	// 没有孤儿时不该误删
	if n, err = st.PruneOrphanDevices(); err != nil || n != 0 {
		t.Fatalf("没有孤儿时应当清 0 台，实际 %d 台 err=%v", n, err)
	}
}

// device_id 为空串的文本不该被当成"没有归属"，否则那台设备会被误清。
//
// 这条是防 NOT IN 那个坑的：只要子查询里出现 NULL，`id NOT IN (...)` 会变成
// NULL、一行都删不掉。现在用的是 NOT EXISTS，空串是普通值，行为明确。
func TestPruneOrphanDevicesWithEmptyDeviceID(t *testing.T) {
	st := newTestStore(t)
	if err := st.TouchDevice("", "ua"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if err := st.CreateText(mkText("t1", "没有归属", "")); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	n, err := st.PruneOrphanDevices()
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("还有一条 device_id 为空的文本，不该清掉那台设备，实际清了 %d 台", n)
	}
}

// ---------------------------------------------------------------- 批量删除

func TestDeleteTextsBatch(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := st.CreateText(mkText(id, "内容-"+id, "dev")); err != nil {
			t.Fatalf("插入 %s 失败: %v", id, err)
		}
	}

	n, err := st.DeleteTexts([]string{"a", "c"})
	if err != nil {
		t.Fatalf("批量删除失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("删除行数 = %d，期望 2", n)
	}
	left, err := st.ListTexts()
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("剩下 %d 条，期望 2", len(left))
	}

	// 不存在的 ID 不该让整批失败：多选删除时某条被别的设备先删掉是正常的，
	// 为它整个报错会让用户莫名其妙。返回值只数真正删掉的。
	n, err = st.DeleteTexts([]string{"b", "no-such-id"})
	if err != nil {
		t.Fatalf("混入不存在的 ID 时不该报错: %v", err)
	}
	if n != 1 {
		t.Fatalf("删除行数 = %d，期望 1（不存在的 ID 不计入）", n)
	}

	// 空列表是空操作，不是错误
	n, err = st.DeleteTexts(nil)
	if err != nil || n != 0 {
		t.Fatalf("空列表：n=%d err=%v，期望 0/nil", n, err)
	}
}

// 超过单批上限时要能跨批删干净——占位符分批那段别写成只处理第一批。
func TestDeleteTextsSpansBatches(t *testing.T) {
	st := newTestStore(t)
	total := deleteBatch + 3
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("t%05d", i)
		ids = append(ids, id)
		if err := st.CreateText(mkText(id, "x", "dev")); err != nil {
			t.Fatalf("插入第 %d 条失败: %v", i, err)
		}
	}

	n, err := st.DeleteTexts(ids)
	if err != nil {
		t.Fatalf("批量删除失败: %v", err)
	}
	if n != total {
		t.Fatalf("删除行数 = %d，期望 %d", n, total)
	}
	left, _ := st.ListTexts()
	if len(left) != 0 {
		t.Fatalf("还剩 %d 条", len(left))
	}
}

// ---------------------------------------------------------------- 过期清理

func TestPurgeTexts(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	mk := func(id string, created, updated time.Time) {
		t.Helper()
		if err := st.CreateText(&Text{
			ID: id, Content: "内容-" + id, DeviceID: "dev",
			CreatedAt: created, UpdatedAt: updated,
		}); err != nil {
			t.Fatalf("插入 %s 失败: %v", id, err)
		}
	}

	mk("old", now.Add(-48*time.Hour), now.Add(-48*time.Hour))
	mk("fresh", now.Add(-time.Hour), now.Add(-time.Hour))
	// 创建很久、但刚刚编辑过。按 created_at 算，它应该被删——
	// 编辑一下就续命会让"1 小时后自动清掉"变得不可预测。
	mk("old-but-edited", now.Add(-48*time.Hour), now)

	n, err := st.PurgeTexts(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("清理行数 = %d，期望 2", n)
	}

	left, _ := st.ListTexts()
	if len(left) != 1 || left[0].ID != "fresh" {
		ids := make([]string, 0, len(left))
		for _, x := range left {
			ids = append(ids, x.ID)
		}
		t.Fatalf("清理后剩下 %v，期望只有 fresh", ids)
	}

	// 边界：before 正好等于创建时间时不该删（用的是严格小于）
	n, err = st.PurgeTexts(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("边界清理失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("before 等于创建时间时删了 %d 条，期望 0", n)
	}
}
