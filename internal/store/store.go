// Package store 负责 QuickShare 的元数据持久化。
//
// 设计要点：
//   - 文件本体落在磁盘目录，数据库只存元数据（文件名、大小、上传时间）。
//   - 使用纯 Go 实现的 modernc.org/sqlite，避免 CGO，方便交叉编译到 NAS。
//   - SQLite 单写者，统一把连接池限制为 1，规避 "database is locked"。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("store: not found")

// Store 封装数据库访问。
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS files (
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
CREATE INDEX IF NOT EXISTS idx_files_fp     ON files(fingerprint, status);
CREATE INDEX IF NOT EXISTS idx_files_status ON files(status);

CREATE TABLE IF NOT EXISTS chunks (
  file_id TEXT    NOT NULL,
  idx     INTEGER NOT NULL,
  size    INTEGER NOT NULL,
  PRIMARY KEY (file_id, idx)
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- 共享文本（便签 / 剪贴板）。内容直接入库：它本来就只有几 KB，
-- 不像文件那样需要把本体挪到磁盘上。
CREATE TABLE IF NOT EXISTS texts (
  id         TEXT    PRIMARY KEY,
  content    TEXT    NOT NULL,
  device_id  TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_texts_created ON texts(created_at);

-- 设备。id 是**发起请求的客户端 IP**，由服务端从连接上推导（不是 UA，也不是
-- 前端传的值）：UA 区分不了两台同型号同版本的手机，拿它当身份会让"改备注"
-- 串到别人身上；前端传的值等于把身份交回请求方。ua 只用来解析出可读的设备名。
-- 设备记录**不随文本删除而消失**，要清掉得显式删，或者打开「设备随消息删除」。
CREATE TABLE IF NOT EXISTS devices (
  id        TEXT    PRIMARY KEY,
  ua        TEXT    NOT NULL DEFAULT '',
  remark    TEXT    NOT NULL DEFAULT '',
  last_seen INTEGER NOT NULL
);
`

// 设置项键名
const (
	SettingTheme     = "theme"              // light | dark | 空（跟随系统）
	SettingBgMime    = "background_mime"    // 背景图的 MIME
	SettingBgVersion = "background_version" // 背景图版本号，用于 URL 破缓存
	SettingBgBlur    = "background_blur"    // 背景图模糊半径（px）

	// 三块区域的不透明度（%）。分开配置是因为顶部栏、上传区、文件列表
	// 在视觉上是三个独立的面板，用户往往只想去调其中一块。
	SettingOpacityTopbar = "opacity_topbar"
	SettingOpacityUpload = "opacity_upload"
	SettingOpacityFiles  = "opacity_files"

	// 文本自动清理：保留多久之后自动删掉。
	//
	// 拆成"数值 + 单位"两个键，而不是统一换算成分钟存起来：后者回填到界面时
	// 要做反算（43200 分钟到底显示成 30 天还是 720 小时），换算边界上会出现
	// "用户填 24 小时、回来变成 1 天"这种困惑。存原样就没有这个问题。
	//
	// 值为空或 0 表示**永不自动删除**——这也是默认。
	SettingTextTTLValue = "text_ttl_value"
	SettingTextTTLUnit  = "text_ttl_unit" // hour | day

	// SettingPruneDevices 控制"设备随消息删除"：打开之后，删文本时顺手把
	// 已经没有任何文本的设备记录也删掉。值 "1" 表示开启，其余（含缺省）关闭。
	//
	// 默认关闭是有意的：设备记录留着，用户给一台设备起好的名字下次还在；
	// 跟着文本删的话，同一个人隔天再来就又变回"未知设备"了。所以这是选项，
	// 不是默认行为。
	SettingPruneDevices = "prune_devices"
)

// Open 打开（并在需要时创建）数据库。
func Open(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化表结构: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// File 是一个已上传（或正在上传）的文件。
type File struct {
	ID          string
	Name        string
	Size        int64
	Mime        string
	Status      string // uploading | ready
	Fingerprint string
	ChunkSize   int64
	TotalChunks int
	CreatedAt   time.Time
}

// ---------------------------------------------------------------- files

// CreateUpload 新建一个上传任务。
func (s *Store) CreateUpload(f *File) error {
	_, err := s.db.Exec(
		`INSERT INTO files (id, name, size, mime, status, fingerprint, chunk_size, total_chunks, created_at)
		 VALUES (?, ?, ?, ?, 'uploading', ?, ?, ?, ?)`,
		f.ID, f.Name, f.Size, f.Mime, f.Fingerprint, f.ChunkSize, f.TotalChunks, f.CreatedAt.Unix(),
	)
	return err
}

// FindResumableUpload 按指纹查找可续传的上传任务。
func (s *Store) FindResumableUpload(fingerprint string) (*File, error) {
	row := s.db.QueryRow(
		`SELECT id, name, size, mime, status, fingerprint, chunk_size, total_chunks, created_at
		   FROM files WHERE fingerprint = ? AND status = 'uploading'
		  ORDER BY created_at DESC, rowid DESC LIMIT 1`, fingerprint)
	return scanFile(row)
}

// GetFile 按 ID 读取文件记录。
func (s *Store) GetFile(id string) (*File, error) {
	row := s.db.QueryRow(
		`SELECT id, name, size, mime, status, fingerprint, chunk_size, total_chunks, created_at
		   FROM files WHERE id = ?`, id)
	return scanFile(row)
}

// ListFiles 列出全部已完成文件，最新的在前。
//
// created_at 只精确到秒，同一秒内连续上传的文件会并列。SQLite 对并列行的
// 顺序不作保证，列表就会"跳"，所以再拿自增的 rowid 兜底：rowid 越大越新。
func (s *Store) ListFiles() ([]*File, error) {
	rows, err := s.db.Query(
		`SELECT id, name, size, mime, status, fingerprint, chunk_size, total_chunks, created_at
		   FROM files WHERE status = 'ready'
		  ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CompleteUpload 把上传任务标记为完成。
func (s *Store) CompleteUpload(id string, realSize int64) error {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`UPDATE files SET status = 'ready', size = ?, completed_at = ?
		  WHERE id = ? AND status = 'uploading'`, realSize, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.Exec(`DELETE FROM chunks WHERE file_id = ?`, id)
	return err
}

// DeleteFile 删除文件记录。
func (s *Store) DeleteFile(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM chunks WHERE file_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// StaleUploads 返回超过 ttl 仍未完成的上传任务。
func (s *Store) StaleUploads(ttl time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	rows, err := s.db.Query(
		`SELECT id FROM files WHERE status = 'uploading' AND created_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanFile(sc interface{ Scan(...any) error }) (*File, error) {
	var f File
	var created int64
	err := sc.Scan(&f.ID, &f.Name, &f.Size, &f.Mime, &f.Status,
		&f.Fingerprint, &f.ChunkSize, &f.TotalChunks, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.CreatedAt = time.Unix(created, 0)
	return &f, nil
}

// ---------------------------------------------------------------- chunks

// AddChunk 记录一个已落盘的分片（重复上传同一分片时覆盖）。
func (s *Store) AddChunk(fileID string, idx int, size int64) error {
	_, err := s.db.Exec(
		`INSERT INTO chunks (file_id, idx, size) VALUES (?, ?, ?)
		 ON CONFLICT(file_id, idx) DO UPDATE SET size = excluded.size`,
		fileID, idx, size)
	return err
}

// ReceivedChunks 返回已收到的分片序号（升序）。
func (s *Store) ReceivedChunks(fileID string) ([]int, error) {
	rows, err := s.db.Query(`SELECT idx FROM chunks WHERE file_id = ? ORDER BY idx`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []int{}
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		out = append(out, idx)
	}
	return out, rows.Err()
}

// ChunkCount 返回已收到的分片数量。
func (s *Store) ChunkCount(fileID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE file_id = ?`, fileID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- 设置

// GetSettings 读取全部设置项。
func (s *Store) GetSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting 写入设置项（不存在则新建）。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteSetting 删除设置项。
func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}

// ---------------------------------------------------------------- 统计

// Stats 返回概览统计。
func (s *Store) Stats() (fileCount int, totalSize int64, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files WHERE status = 'ready'`).
		Scan(&fileCount, &totalSize)
	return
}

// maxNameBytes 是文件名的字节上限。
const maxNameBytes = 200

// SanitizeName 清理用户提供的文件名，防止路径穿越与非法字符。
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")

	// 截断必须落在字符边界上。早先直接写 `name[:200]`——那是按**字节**切，
	// 一个汉字占 3 字节，切在第 200 字节就会把它劈成半个，落库是非法 UTF-8，
	// 页面上显示成一个 � 方块。这里按字符累计长度，宁可少几个字节也不切碎字符。
	if len(name) > maxNameBytes {
		var b strings.Builder
		for _, r := range name {
			if b.Len()+utf8.RuneLen(r) > maxNameBytes {
				break
			}
			b.WriteRune(r)
		}
		name = strings.Trim(b.String(), ". ")
	}

	if name == "" {
		name = "unnamed"
	}
	return name
}
