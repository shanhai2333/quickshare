package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"quickshare/internal/store"
)

// dbName 是元数据库在数据目录里的固定文件名。
const dbName = "quickshare.db"

// dataItems 是数据目录里属于本服务的条目。
//
// 迁移时只搬这几项，而不是整目录递归：用户很可能把数据目录指向一个
// 还放着别的东西的大目录（比如 /volume1），全量复制会莫名其妙拷走一堆无关文件。
var dataItems = []string{
	"files", "chunks", "background",
	dbName, dbName + "-wal", dbName + "-shm",
}

// storageDTO 描述当前数据目录，以及能不能在界面上改。
type storageDTO struct {
	DataDir string `json:"dataDir"`
	Locked  bool   `json:"locked"` // 由启动参数/环境变量决定，界面只读
	Reason  string `json:"reason,omitempty"`
}

// storageResult 是一次目录切换的结果。
type storageResult struct {
	DataDir  string `json:"dataDir"`
	Migrated bool   `json:"migrated"`
	Files    int    `json:"files"` // 迁移过去的文件数
	// LeftBehind 非空表示旧目录里的数据没有删除，需要用户自己清理。
	// 只有"同盘 rename"这一条路径能真正做到搬走，其余都会留下原件。
	LeftBehind string `json:"leftBehind,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// handleGetStorage 返回当前数据目录。需要管理权限——文件系统路径
// 属于部署信息，不该让未鉴权的访问者看到 NAS 的目录结构。
func (s *Server) handleGetStorage(w http.ResponseWriter, r *http.Request) {
	abs, err := filepath.Abs(s.be().dataDir)
	if err != nil {
		abs = s.be().dataDir
	}
	dto := storageDTO{DataDir: abs}
	if s.cfg.DataDirLocked {
		dto.Locked = true
		dto.Reason = "数据目录由启动参数或环境变量指定，请在部署配置里修改"
	} else if s.cfg.PersistDataDir == nil {
		dto.Locked = true
		dto.Reason = "当前部署方式无法保存该设置"
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePutStorage 切换数据目录，可选择性迁移已有文件。
func (s *Server) handlePutStorage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path    string `json:"path"`
		Migrate bool   `json:"migrate"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	if s.cfg.DataDirLocked {
		writeErr(w, http.StatusConflict,
			"数据目录由启动参数或环境变量指定，请在部署配置（如 docker-compose 的卷映射）里修改")
		return
	}
	if s.cfg.PersistDataDir == nil {
		writeErr(w, http.StatusConflict, "当前部署方式无法保存该设置")
		return
	}

	path := strings.TrimSpace(in.Path)
	if path == "" {
		writeErr(w, http.StatusBadRequest, "目录不能为空")
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "路径不合法："+err.Error())
		return
	}

	res, err := s.switchDataDir(abs, in.Migrate)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// switchDataDir 内部的 defer 已经把 switching 复位了，此刻其它请求不再被
	// 503 挡住，通知出去的回拉才拉得到东西。顺序反了前端就会收到一次 503。
	s.events.notify(topicFiles)
	writeJSON(w, http.StatusOK, res)
}

// switchDataDir 把服务切到新的数据目录。
//
// 顺序刻意是"先确认新目录能用、再整体换指针"：中间任何一步失败都退回旧后端，
// 绝不让服务停在"数据库已关闭、又没有新库"的状态上。
func (s *Server) switchDataDir(newDir string, migrate bool) (storageResult, error) {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	old := s.be()
	oldAbs, err := filepath.Abs(old.dataDir)
	if err != nil {
		oldAbs = old.dataDir
	}
	newDir = filepath.Clean(newDir)

	if oldAbs == newDir {
		return storageResult{}, errors.New("新目录与当前目录相同")
	}
	// 互相嵌套会带来一堆麻烦：迁移时把自己拷进自己，清理时误删新目录
	if isSubPath(oldAbs, newDir) || isSubPath(newDir, oldAbs) {
		return storageResult{}, errors.New("新目录不能位于当前数据目录内部，也不能反过来包含它")
	}

	// 先探一下能不能写。只检查权限位不够——NAS 上只读挂载、磁盘满、
	// SMB 权限都能让写入失败，而报错点离这里很远，排查起来很费劲。
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return storageResult{}, fmt.Errorf("无法创建目录：%w", err)
	}
	if err := probeWritable(newDir); err != nil {
		return storageResult{}, fmt.Errorf("目录不可写：%w", err)
	}

	var leftBehind string
	if migrate {
		if err := ensureNoExistingData(newDir); err != nil {
			return storageResult{}, err
		}
		// 关库之前先挡住请求：这段时间数据库是关着的，
		// 放请求进去只会拿到一堆 "database is closed"
		s.switching.Store(true)
		defer s.switching.Store(false)

		if err := old.st.Close(); err != nil {
			return storageResult{}, fmt.Errorf("关闭旧数据库失败：%w", err)
		}
		moved, left, err := moveDataDir(oldAbs, newDir)
		if err != nil {
			s.recover(oldAbs)
			return storageResult{}, fmt.Errorf("迁移数据失败：%w", err)
		}
		leftBehind = left
		_ = moved
	}

	newSt, err := store.Open(filepath.Join(newDir, dbName))
	if err != nil {
		if migrate {
			s.recover(oldAbs)
		}
		return storageResult{}, fmt.Errorf("打开新目录的数据库失败：%w", err)
	}

	// 新库可用，整体替换。此刻起所有请求都走新目录。
	s.cur.Store(&backend{st: newSt, dataDir: newDir})

	// 不迁移时旧库一直开着，这里补一刀关掉，免得句柄和 WAL 文件一直挂着。
	// 迁移那条路径在上面关库前就关过了。
	if !migrate {
		_ = old.st.Close()
	}

	res := storageResult{DataDir: newDir, Migrated: migrate, LeftBehind: leftBehind}
	if n, err := countFiles(filepath.Join(newDir, "files")); err == nil {
		res.Files = n
	}

	if err := s.cfg.PersistDataDir(newDir); err != nil {
		// 目录已经切过去了，只是下次启动会回到旧目录。如实告诉用户。
		res.Warning = "已切换，但写入启动配置失败，重启后会回到原目录：" + err.Error()
	}
	return res, nil
}

// recover 迁移失败后把旧数据库重新打开，让服务继续可用。
func (s *Server) recover(oldDir string) {
	st, err := store.Open(filepath.Join(oldDir, dbName))
	if err != nil {
		return // 旧库也打不开了，只能靠重启
	}
	s.cur.Store(&backend{st: st, dataDir: oldDir})
}

// moveDataDir 把数据从 from 搬到 to。
//
// 同一文件系统上直接 rename，瞬时且没有"拷到一半"的中间态；跨文件系统
// （NAS 上两块盘之间很常见）才真的复制。复制这条路**不删原件**——
// 搬完再把原目录删掉是不可逆的，万一复制过程中出了问题就找不回来了。
// 返回值 leftBehind 非空时，提醒用户旧数据还在，可以自行清理。
func moveDataDir(from, to string) (moved int, leftBehind string, err error) {
	// rename 要求目标不存在（Windows 上尤其严格），先把刚建出来的空目录收掉
	if fi, statErr := os.Stat(to); statErr == nil && fi.IsDir() {
		if rmErr := os.Remove(to); rmErr != nil {
			return 0, "", fmt.Errorf("目标目录不是空的，无法整体搬迁：%w", rmErr)
		}
	}
	if renameErr := os.Rename(from, to); renameErr == nil {
		n, _ := countFiles(filepath.Join(to, "files"))
		return n, "", nil // 同盘：原件已经跟着搬过去了，没有遗留
	}

	// 跨文件系统：真拷
	if err := os.MkdirAll(to, 0o755); err != nil {
		return 0, "", err
	}
	if err := copyDataItems(from, to); err != nil {
		return 0, "", err
	}
	n, _ := countFiles(filepath.Join(to, "files"))
	return n, from, nil
}

// copyDataItems 复制数据目录里属于本服务的那几项。
func copyDataItems(from, to string) error {
	for _, name := range dataItems {
		src := filepath.Join(from, name)
		fi, err := os.Stat(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 没设过背景图、没有残留分片，都属正常
			}
			return err
		}
		dst := filepath.Join(to, name)
		if fi.IsDir() {
			if err := copyTree(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := copyOne(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyOne(p, target)
	})
}

func copyOne(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// probeWritable 往目录里写一个探针文件再删掉，确认真的可写。
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".qs-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// ensureNoExistingData 迁移前确认目标目录里没有另一个实例的数据，
// 免得把人家的文件覆盖掉。
func ensureNoExistingData(dir string) error {
	for _, name := range []string{dbName, "files", "chunks"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("目标目录里已存在 %s，迁移会覆盖它。请换一个空目录", name)
		}
	}
	return nil
}

func countFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// isSubPath 判断 child 是否位于 parent 之内（含相等不算）。
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}
