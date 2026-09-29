package admin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/api"
)

// 简化版分片上传：只服务主题(theme)上传场景，
// 兼容官方前端 chunkUpload.ts 的 /init /chunk /merge /cancel 协议。

const themeUploadChunkSize = 5 * 1024 * 1024 // 必须和前端 CHUNK_SIZE 保持一致

type themeUploadSession struct {
	mu         sync.Mutex
	uploadID   string
	filename   string
	size       int64
	totalChunk int
	dir        string
	received   map[int]bool
	createdAt  time.Time
}

var (
	themeUploadMu       sync.Mutex
	themeUploadSessions = map[string]*themeUploadSession{}
)

func generateUploadID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// UploadInit 对应 POST /api/admin/upload/init
func UploadInit(c *gin.Context) {
	var req struct {
		Purpose  string `json:"purpose"`
		Size     int64  `json:"size"`
		Filename string `json:"filename"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}

	// 简化版仅支持主题上传
	if req.Purpose != "theme" {
		api.RespondError(c, http.StatusBadRequest, "当前仅支持主题(theme)上传")
		return
	}
	if req.Size <= 0 {
		api.RespondError(c, http.StatusBadRequest, "文件大小无效")
		return
	}

	id, err := generateUploadID()
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "生成上传ID失败: "+err.Error())
		return
	}

	dir := filepath.Join(os.TempDir(), "komari-theme-upload", id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		api.RespondError(c, http.StatusInternalServerError, "创建临时目录失败: "+err.Error())
		return
	}

	total := int((req.Size + themeUploadChunkSize - 1) / themeUploadChunkSize)

	session := &themeUploadSession{
		uploadID:   id,
		filename:   req.Filename,
		size:       req.Size,
		totalChunk: total,
		dir:        dir,
		received:   map[int]bool{},
		createdAt:  time.Now(),
	}

	themeUploadMu.Lock()
	themeUploadSessions[id] = session
	themeUploadMu.Unlock()

	api.RespondSuccess(c, gin.H{
		"upload_id":  id,
		"chunk_size": themeUploadChunkSize,
	})
}

// UploadChunk 对应 POST /api/admin/upload/chunk
func UploadChunk(c *gin.Context) {
	uploadID := c.PostForm("upload_id")
	chunkIndexStr := c.PostForm("chunk_index")

	if uploadID == "" || chunkIndexStr == "" {
		api.RespondError(c, http.StatusBadRequest, "缺少必要参数")
		return
	}
	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	if err != nil || chunkIndex < 0 {
		api.RespondError(c, http.StatusBadRequest, "分片序号无效")
		return
	}

	themeUploadMu.Lock()
	session, ok := themeUploadSessions[uploadID]
	themeUploadMu.Unlock()
	if !ok {
		api.RespondError(c, http.StatusBadRequest, "上传任务不存在或已过期")
		return
	}

	fileHeader, err := c.FormFile("chunk_data")
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, "读取分片数据失败: "+err.Error())
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "打开分片数据失败: "+err.Error())
		return
	}
	defer src.Close()

	chunkPath := filepath.Join(session.dir, fmt.Sprintf("chunk_%d", chunkIndex))
	out, err := os.OpenFile(chunkPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "保存分片失败: "+err.Error())
		return
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		api.RespondError(c, http.StatusInternalServerError, "写入分片失败: "+err.Error())
		return
	}
	out.Close()

	session.mu.Lock()
	session.received[chunkIndex] = true
	session.mu.Unlock()

	api.RespondSuccess(c, nil)
}

// UploadMerge 对应 POST /api/admin/upload/merge，主题的安装逻辑在这里触发
func UploadMerge(c *gin.Context) {
	var req struct {
		UploadID string `json:"upload_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}

	themeUploadMu.Lock()
	session, ok := themeUploadSessions[req.UploadID]
	themeUploadMu.Unlock()
	if !ok {
		api.RespondError(c, http.StatusBadRequest, "上传任务不存在或已过期")
		return
	}

	session.mu.Lock()
	for i := 0; i < session.totalChunk; i++ {
		if !session.received[i] {
			session.mu.Unlock()
			api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("分片 %d 缺失，无法合并", i))
			return
		}
	}
	session.mu.Unlock()

	mergedPath := filepath.Join(session.dir, "merged.zip")
	out, err := os.OpenFile(mergedPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "创建合并文件失败: "+err.Error())
		return
	}
	for i := 0; i < session.totalChunk; i++ {
		chunkPath := filepath.Join(session.dir, fmt.Sprintf("chunk_%d", i))
		in, err := os.Open(chunkPath)
		if err != nil {
			out.Close()
			api.RespondError(c, http.StatusInternalServerError, fmt.Sprintf("读取分片 %d 失败: %v", i, err))
			return
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			out.Close()
			api.RespondError(c, http.StatusInternalServerError, fmt.Sprintf("合并分片 %d 失败: %v", i, err))
			return
		}
	}
	out.Close()

	// 复用现有的、已验证可用的主题解压安装逻辑
	themeInfo, err := extractAndValidateTheme(mergedPath)

	// 无论成功失败都清理临时目录和会话
	themeUploadMu.Lock()
	delete(themeUploadSessions, req.UploadID)
	themeUploadMu.Unlock()
	os.RemoveAll(session.dir)

	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}

	api.RespondSuccessMessage(c, "主题上传成功", themeInfo)
}

// UploadCancel 对应 POST /api/admin/upload/cancel
func UploadCancel(c *gin.Context) {
	var req struct {
		UploadID string `json:"upload_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}

	themeUploadMu.Lock()
	session, ok := themeUploadSessions[req.UploadID]
	if ok {
		delete(themeUploadSessions, req.UploadID)
	}
	themeUploadMu.Unlock()

	if ok {
		os.RemoveAll(session.dir)
	}

	api.RespondSuccess(c, nil)
}
