package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"quickshare/internal/store"
)

// handleUploadInit 创建一个分片上传任务；若存在同名的未完成任务则直接续传。
func (s *Server) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	var req struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Mime string `json:"mime"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	name := store.SanitizeName(req.Name)
	if req.Size < 0 {
		writeErr(w, http.StatusBadRequest, "文件大小不合法")
		return
	}
	if s.cfg.MaxFileSize > 0 && req.Size > s.cfg.MaxFileSize {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("文件超过上限 %s", humanSize(s.cfg.MaxFileSize)))
		return
	}

	chunkSize := s.chunkSize()
	total := int((req.Size + chunkSize - 1) / chunkSize)
	fingerprint := fingerprintOf(name, req.Size)

	// 尝试续传
	if existing, err := b.st.FindResumableUpload(fingerprint); err == nil {
		received, _ := b.st.ReceivedChunks(existing.ID)
		writeJSON(w, http.StatusOK, map[string]any{
			"uploadId":    existing.ID,
			"chunkSize":   existing.ChunkSize,
			"totalChunks": existing.TotalChunks,
			"received":    received,
			"resumed":     true,
		})
		return
	}

	id, err := randomToken(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成上传 ID 失败")
		return
	}
	mimeType := req.Mime
	if mimeType == "" {
		mimeType = mimeByExt(name)
	}
	// 文本类文件一律按 text/plain 存：既能看源码，又不会被当 HTML/XML 渲染。
	// 表与理由都在 download.go 的 textExts 那段注释里。
	mimeType = normalizeUploadMime(name, mimeType)

	f := &store.File{
		ID:          id,
		Name:        name,
		Size:        req.Size,
		Mime:        mimeType,
		Fingerprint: fingerprint,
		ChunkSize:   chunkSize,
		TotalChunks: total,
		CreatedAt:   time.Now(),
	}
	if err := b.st.CreateUpload(f); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(b.chunkDir(id), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建分片目录失败")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uploadId":    id,
		"chunkSize":   chunkSize,
		"totalChunks": total,
		"received":    []int{},
		"resumed":     false,
	})
}

// handleUploadChunk 接收单个分片。请求体即分片原始字节。
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	id := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 {
		writeErr(w, http.StatusBadRequest, "分片序号不合法")
		return
	}

	f, err := b.st.GetFile(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "上传任务不存在")
		return
	}
	if f.Status != "uploading" {
		writeErr(w, http.StatusConflict, "该上传已完成")
		return
	}
	// 越界检查必须无条件生效。这里早先写的是 `f.TotalChunks > 0 && idx >= f.TotalChunks`，
	// 于是 totalChunks=0（声明 0 字节的文件）时整个检查被跳过：可以往这个任务里
	// 无限塞 8 MiB 分片，每个都返回 200 并落盘，QS_MAX_FILE_SIZE 形同虚设。
	// 0 字节文件本来就没有分片，任何分片都该拒。
	if idx >= f.TotalChunks {
		writeErr(w, http.StatusBadRequest, "分片序号越界")
		return
	}

	// 允许 64KiB 的余量，防止客户端计算误差
	body := http.MaxBytesReader(w, r.Body, f.ChunkSize+64<<10)
	defer func() { _ = r.Body.Close() }()

	if err := os.MkdirAll(b.chunkDir(id), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建分片目录失败")
		return
	}

	final := b.chunkPath(id, idx)
	tmp := final + ".tmp"

	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "写入分片失败")
		return
	}
	n, err := io.Copy(dst, body)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "分片体积超出预期")
			return
		}
		writeErr(w, http.StatusInternalServerError, "写入分片失败: "+err.Error())
		return
	}

	// 逐片校验长度。这样一旦某片被截断，能立刻定位到具体分片并让客户端重传，
	// 而不是等到合并阶段才发现整体对不上（那时上传任务已经无法恢复）。
	if want := s.expectedChunkSize(f, idx); n != want {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("分片 %d 大小不符（期望 %d 字节，实际 %d 字节）", idx, want, n))
		return
	}

	// 原子落盘：先写临时文件再改名，避免半截分片被误判为已完成
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "分片落盘失败")
		return
	}
	if err := b.st.AddChunk(id, idx, n); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	count, _ := b.st.ChunkCount(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": count, "total": f.TotalChunks})
}

// handleUploadStatus 返回已收到的分片，供客户端断点续传。
func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	id := r.PathValue("id")
	f, err := b.st.GetFile(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "上传任务不存在")
		return
	}
	received, err := b.st.ReceivedChunks(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uploadId":    f.ID,
		"name":        f.Name,
		"size":        f.Size,
		"chunkSize":   f.ChunkSize,
		"totalChunks": f.TotalChunks,
		"received":    received,
		"status":      f.Status,
	})
}

// handleUploadComplete 合并分片并生成正式文件。
func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	id := r.PathValue("id")
	f, err := b.st.GetFile(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "上传任务不存在")
		return
	}
	if f.Status == "ready" {
		writeJSON(w, http.StatusOK, fileDTO(f))
		return
	}

	received, err := b.st.ReceivedChunks(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(received) < f.TotalChunks {
		missing := missingChunks(received, f.TotalChunks)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "分片不完整，无法合并",
			"missing": missing,
		})
		return
	}

	if err := os.MkdirAll(filepath.Dir(b.filePath(id)), 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建存储目录失败")
		return
	}

	written, err := s.mergeChunks(b, id, f.TotalChunks)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "合并分片失败: "+err.Error())
		return
	}
	// 不写 `f.Size > 0 &&` 这个前缀：0 字节文件同样要校验（合并结果必须是 0 字节），
	// 否则声明 0 字节就成了绕过一切大小检查的入口。
	if written != f.Size {
		// 逐片校验已经拦住了绝大多数情况；走到这里说明分片在磁盘上被外部改动过。
		// 直接重置这个上传任务，避免客户端卡在一个永远无法完成的状态里。
		_ = os.Remove(b.filePath(id))
		_ = b.st.DeleteFile(id)
		_ = os.RemoveAll(b.chunkDir(id))
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("合并后大小不符（期望 %d 字节，实际 %d 字节），已重置该上传任务，请重新上传", f.Size, written))
		return
	}

	if err := b.st.CompleteUpload(id, written); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(b.chunkDir(id))

	// 通知在**落库之后**才发：前端收到通知会立刻回拉列表，
	// 如果先发通知再落库，回拉到的就是旧列表，文件会"慢一拍"才出现。
	s.events.notify(topicFiles)

	f.Status = "ready"
	f.Size = written
	writeJSON(w, http.StatusOK, fileDTO(f))
}

// handleUploadCancel 取消上传并清理分片。
func (s *Server) handleUploadCancel(w http.ResponseWriter, r *http.Request) {
	b := s.be()
	id := r.PathValue("id")
	if err := b.st.DeleteFile(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(b.chunkDir(id))
	_ = os.Remove(b.filePath(id))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 辅助

// mergeChunks 按序拼接分片，返回写入的总字节数。
func (s *Server) mergeChunks(b *backend, id string, total int) (int64, error) {
	dst, err := os.OpenFile(b.filePath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() { _ = dst.Close() }()

	var written int64
	for i := 0; i < total; i++ {
		src, err := os.Open(b.chunkPath(id, i))
		if err != nil {
			return written, fmt.Errorf("打开分片 %d: %w", i, err)
		}
		n, err := io.Copy(dst, src)
		_ = src.Close()
		if err != nil {
			return written, fmt.Errorf("拷贝分片 %d: %w", i, err)
		}
		written += n
	}
	if err := dst.Sync(); err != nil {
		return written, err
	}
	return written, nil
}

// expectedChunkSize 返回第 idx 个分片应有的字节数（末片通常不满）。
func (s *Server) expectedChunkSize(f *store.File, idx int) int64 {
	if f.TotalChunks > 0 && idx == f.TotalChunks-1 {
		return f.Size - int64(idx)*f.ChunkSize
	}
	return f.ChunkSize
}

func missingChunks(received []int, total int) []int {
	have := make(map[int]bool, len(received))
	for _, i := range received {
		have[i] = true
	}
	missing := []int{}
	for i := 0; i < total; i++ {
		if !have[i] {
			missing = append(missing, i)
		}
	}
	return missing
}

func fingerprintOf(name string, size int64) string {
	sum := sha256.Sum256([]byte(name + "\x00" + strconv.FormatInt(size, 10)))
	return hex.EncodeToString(sum[:16])
}

func mimeByExt(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func fileDTO(f *store.File) map[string]any {
	return map[string]any{
		"id": f.ID, "name": f.Name, "size": f.Size,
		"mime": f.Mime, "createdAt": f.CreatedAt.Unix(),
		// 和 GET /api/files 保持同一个字段（前端两处都读它决定要不要给「预览」按钮）
		"preview": previewKindOf(f.Mime),
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
