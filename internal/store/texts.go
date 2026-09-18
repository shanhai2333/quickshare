package store

import (
	"database/sql"
	"strings"
	"time"
)

// Text 是一条共享文本（便签 / 剪贴板）。
//
// 内容直接存库，不像文件那样把本体挪到磁盘：它只有几 KB，
// 而"存库"换来的是编辑、删除、排序都能用一条 SQL 表达。
type Text struct {
	ID        string
	Content   string
	DeviceID  string // 发送方的设备 ID（客户端 IP，由服务端从连接上推导）
	CreatedAt time.Time
	UpdatedAt time.Time
	// TTLSeconds 是这条文本自己的保留时长。0 = 跟随全局设置。
	TTLSeconds int64
	// IsCode 表示插入时判定为"像验证码"。判定逻辑在 server 的 looksLikeCode，
	// 这里只负责存；清理时用它决定走验证码规则还是普通文本规则。
	IsCode bool
}

// Device 是一台用过的设备。
//
// ID 是**发起请求的客户端 IP**，由服务端从连接上推导，不是前端传的。
//
// 早先这里存的是浏览器生成的随机串（放在 localStorage 里）。那个方案在同一台
// 机器换地址访问时会分裂：localStorage 严格按 origin 隔离，127.0.0.1 /
// localhost / 内网 IP 各存一份，同一台电脑就被记成了好几台设备。改用 IP 之后
// 这些地址归一到同一台。代价是同一个 NAT 后面共享出口 IP 的设备会被合并
// ——内网直连场景下很少见。
//
// UA **不是身份**，只用来解析出可读的设备名做默认显示：两台同型号同版本的
// 手机 UA 一模一样，拿它当身份会让"改备注"改到别人头上。
type Device struct {
	ID       string
	UA       string
	Remark   string // 用户改的备注；为空时前端显示 UA 解析出来的名字
	LastSeen time.Time
	// TextCount 是这台设备当前还留着几条文本。
	//
	// 设备记录**不随文本删除而消失**（删文本时顺手清设备会误伤：同一台设备
	// 可能刚发完一条、上一条刚好被删）。所以界面上要能看出"这台设备其实已经
	// 空了"，用户才好决定要不要删掉它。
	TextCount int
}

// ---------------------------------------------------------------- texts

func scanText(row interface{ Scan(...any) error }) (*Text, error) {
	var t Text
	var created, updated int64
	var isCode int
	if err := row.Scan(&t.ID, &t.Content, &t.DeviceID, &created, &updated,
		&t.TTLSeconds, &isCode); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	t.IsCode = isCode == 1
	return &t, nil
}

const textCols = `id, content, device_id, created_at, updated_at, ttl_seconds, is_code`

// ListTexts 列出全部文本，最新的在前。
//
// created_at 只精确到秒，同一秒内连发几条会并列；SQLite 对并列行的顺序
// 不作保证，列表就会"跳"。所以再拿自增的 rowid 兜底（和文件列表同一个坑）。
func (s *Store) ListTexts() ([]*Text, error) {
	rows, err := s.db.Query(
		`SELECT ` + textCols + ` FROM texts ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Text{}
	for rows.Next() {
		t, err := scanText(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetText 按 ID 读取一条文本。
func (s *Store) GetText(id string) (*Text, error) {
	row := s.db.QueryRow(`SELECT `+textCols+` FROM texts WHERE id = ?`, id)
	return scanText(row)
}

// CreateText 插入一条文本。
func (s *Store) CreateText(t *Text) error {
	_, err := s.db.Exec(
		`INSERT INTO texts (id, content, device_id, created_at, updated_at, ttl_seconds, is_code)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Content, t.DeviceID, t.CreatedAt.Unix(), t.UpdatedAt.Unix(),
		t.TTLSeconds, boolToInt(t.IsCode),
	)
	return err
}

// SetTextTTL 改一条文本自己的保留时长。0 表示改回"跟随全局设置"。
func (s *Store) SetTextTTL(id string, seconds int64) error {
	return s.setTTL("texts", id, seconds)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpdateText 改内容，同时推进 updated_at（列表里要显示"已编辑"）。
//
// isCode 一起写：内容变了，"像不像验证码"的判定结果可能跟着变。判定逻辑在
// server 那边（store 不做内容判断），这里只负责存。
func (s *Store) UpdateText(id, content string, isCode bool) error {
	res, err := s.db.Exec(
		`UPDATE texts SET content = ?, updated_at = ?, is_code = ? WHERE id = ?`,
		content, time.Now().Unix(), boolToInt(isCode), id,
	)
	if err != nil {
		return err
	}
	// 改一个不存在的 ID 不会报错，只会影响 0 行——必须自己查，否则
	// 接口会对着一个不存在的记录回 200，前端以为改成功了。
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteText 删除一条文本。
func (s *Store) DeleteText(id string) error {
	res, err := s.db.Exec(`DELETE FROM texts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- devices

// TouchDevice 记录一台设备"来过"，但**不动它的备注**。
//
// 用 upsert 而不是"先查再插"：这里每次请求都会调，两条语句在并发下
// 会撞出 UNIQUE 冲突。ON CONFLICT 的 DO UPDATE 里刻意不写 remark，
// 这样用户改过的备注不会被后续请求覆盖回空。
func (s *Store) TouchDevice(id, ua string) error {
	_, err := s.db.Exec(
		`INSERT INTO devices (id, ua, remark, last_seen) VALUES (?, ?, '', ?)
		 ON CONFLICT(id) DO UPDATE SET ua = excluded.ua, last_seen = excluded.last_seen`,
		id, ua, time.Now().Unix(),
	)
	return err
}

// ListDevices 列出全部设备，最近活跃的在前，并带上各自还剩几条文本。
//
// 条数用相关子查询带出来，不另发一轮查询：设备数量本来就不多（内网自用），
// 一次扫完比"列完再逐台数"少一个来回，也不会出现两次查询之间数据变了、
// 界面上的数字对不上的情况。
func (s *Store) ListDevices() ([]*Device, error) {
	rows, err := s.db.Query(
		`SELECT d.id, d.ua, d.remark, d.last_seen,
		        (SELECT COUNT(*) FROM texts t WHERE t.device_id = d.id)
		   FROM devices d
		  ORDER BY d.last_seen DESC, d.rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Device{}
	for rows.Next() {
		var d Device
		var seen int64
		if err := rows.Scan(&d.ID, &d.UA, &d.Remark, &seen, &d.TextCount); err != nil {
			return nil, err
		}
		d.LastSeen = time.Unix(seen, 0)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// DeleteDevice 删掉一台设备的记录（含备注）。
//
// **只删设备记录，不碰它的文本**：文本是用户要留的内容，设备记录只是个
// 显示名 + 备注的载体。删完之后那些文本还在，显示名回落成"未知设备"——
// 界面上确认删除前会说清这一点。
func (s *Store) DeleteDevice(id string) error {
	res, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	// SQLite 对"删了 0 行"不报错，不转成 ErrNotFound 的话接口会对着不存在的
	// 设备回 200，前端以为删成功了。
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneOrphanDevices 删掉"已经没有任何文本"的设备记录，返回删掉几台。
//
// 这是「设备随消息删除」那个设置的实现。默认**不开**：设备记录留着能让用户
// 给一台设备起好名字、下次它再发文本时名字还在；一旦跟着文本删，同一个人
// 隔天再来就又是"未知设备"了。所以做成开关而不是默认行为。
//
// 用 NOT EXISTS 而不是 `id NOT IN (SELECT device_id ...)`：NOT IN 只要子查询
// 里出现一个 NULL，整个条件就变成 NULL、一行都删不掉。device_id 现在是
// NOT NULL 且默认空串，但这类"以后加个可空列就悄悄失效"的写法不值得赌。
// （顺带一句：gofmt 会把注释里的两个单引号改成弯引号，所以上面没写成字面量。）
func (s *Store) PruneOrphanDevices() (int, error) {
	res, err := s.db.Exec(
		`DELETE FROM devices
		  WHERE NOT EXISTS (SELECT 1 FROM texts t WHERE t.device_id = devices.id)`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// SetDeviceRemark 改设备备注。空串表示恢复成"用 UA 解析出来的名字"。
func (s *Store) SetDeviceRemark(id, remark string) error {
	res, err := s.db.Exec(`UPDATE devices SET remark = ? WHERE id = ?`, remark, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- 批量与清理

// deleteBatch 单条 DELETE 里最多绑多少个 ID。
//
// SQLite 的绑定变量数有上限（老版本 999、新版本 32766），一次塞几百个虽然
// 通常没问题，但没必要去赌对方编译时用的是哪个宏。分批也不慢。
const deleteBatch = 500

// DeleteTexts 批量删除，返回实际删掉的行数。
//
// 用一条 `DELETE ... WHERE id IN (...)` 而不是循环调 DeleteText：
// 循环是几十次往返，而且中途失败会留下"删了一半"的状态——前端已经把这些
// 条目从界面上抹掉了，用户根本不知道哪几条还在。
//
// 刻意**不**因为某个 ID 不存在而报错：多选删除的场景下，某条恰好被别的设备
// 先删掉了是很正常的，为它整个失败会让用户莫名其妙。返回值告诉调用方实际删了几条。
func (s *Store) DeleteTexts(ids []string) (int, error) {
	total := 0
	for start := 0; start < len(ids); start += deleteBatch {
		end := start + deleteBatch
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]

		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			ph[i] = "?"
			args[i] = id
		}
		res, err := s.db.Exec(
			`DELETE FROM texts WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(n)
	}
	return total, nil
}

// PurgeTexts 按每条文本自己的规则删掉已过期的，返回删除条数。
//
// 优先级：**单条 ttl_seconds > 验证码规则 > 全局 text_ttl**。
// 单条排最前是有意的：用户为某一条显式指定了时长，那就不该再被"它长得像验证码"
// 或者全局设置覆盖掉。
//
// 按 created_at 算，**不是 updated_at**：用户配的是"这条文本留多久"，
// 如果编辑一下就续命，"1 小时后自动清掉"就变得不可预测了。
//
// textSec / codeSec <= 0 表示那一档没启用。这里把"没启用"折算成截止时刻 0
// ——created_at 是正的 unix 秒，`created_at < 0` 恒为假，正好等于这一档不删。
// 这样一条 SQL 就能表达三档，不用把行拉回 Go 里逐条判断。
func (s *Store) PurgeTexts(now time.Time, textSec, codeSec int64) (int, error) {
	codeCut, textCut := int64(0), int64(0)
	if codeSec > 0 {
		codeCut = now.Unix() - codeSec
	}
	if textSec > 0 {
		textCut = now.Unix() - textSec
	}

	res, err := s.db.Exec(
		`DELETE FROM texts WHERE
		   (ttl_seconds > 0 AND created_at + ttl_seconds < ?)
		   OR (ttl_seconds = 0 AND is_code = 1 AND created_at < ?)
		   OR (ttl_seconds = 0 AND is_code = 0 AND created_at < ?)`,
		now.Unix(), codeCut, textCut)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
