package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sync4loong/pkg/cache"
	"sync4loong/pkg/config"
	"sync4loong/pkg/logger"
	"sync4loong/pkg/publisher"
)

type HTTPHandler struct {
	publisher *publisher.Publisher
	cache     *cache.FileExistenceCache
	logger    *logger.Logger
}

type PublishRequest struct {
	FolderPath string `json:"folder_path"`
	Prefix     string `json:"prefix"`
}

type PublishResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

type CheckResponse struct {
	Exists          bool     `json:"exists"`
	Cached          bool     `json:"cached"`
	Size            int64    `json:"size,omitempty"`
	Timestamp       string   `json:"timestamp"`
	S3Key           string   `json:"s3_key"`
	AllowedPrefixes []string `json:"allowed_prefixes,omitempty"`
}

type ErrorResponse struct {
	Error           string   `json:"error"`
	AllowedPrefixes []string `json:"allowed_prefixes,omitempty"`
}

type ClearCacheResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

func NewHTTPHandler(config *config.Config, cache *cache.FileExistenceCache) (*HTTPHandler, error) {
	pub, err := publisher.NewPublisher(config)
	if err != nil {
		return nil, err
	}

	return &HTTPHandler{
		publisher: pub,
		cache:     cache,
		logger:    logger.NewDefault(),
	}, nil
}

func (h *HTTPHandler) Close() {
	if h.publisher != nil {
		h.publisher.Close()
	}
}

func (h *HTTPHandler) PublishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.sendErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req PublishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendErrorResponse(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	if req.FolderPath == "" {
		h.sendErrorResponse(w, http.StatusBadRequest, "folder_path is required")
		return
	}

	if req.Prefix == "" {
		h.sendErrorResponse(w, http.StatusBadRequest, "prefix is required")
		return
	}

	if err := h.publisher.PublishFileSyncTask(req.FolderPath, req.Prefix); err != nil {
		h.logger.Error("failed to publish task", err, map[string]any{
			"folder_path": req.FolderPath,
			"prefix":      req.Prefix,
		})
		h.sendErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.logger.Info("task published via HTTP", map[string]any{
		"folder_path": req.FolderPath,
		"prefix":      req.Prefix,
	})

	response := PublishResponse{
		Success: true,
		Message: "task published successfully",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode response", err, nil)
	}
}

func (h *HTTPHandler) sendErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	response := PublishResponse{
		Success: false,
		Error:   message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode error response", err, nil)
	}
}

func (h *HTTPHandler) CheckFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.sendErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var s3Key string

	path := r.URL.Path
	if strings.HasPrefix(path, "/check/") {
		s3Key = strings.TrimPrefix(path, "/check/")
	} else {
		s3Key = r.URL.Query().Get("key")
	}

	if s3Key == "" {
		h.sendCheckErrorResponse(w, http.StatusBadRequest, "s3 key is required")
		return
	}

	result, err := h.cache.CheckFileExists(r.Context(), s3Key)
	if err != nil {
		if strings.Contains(err.Error(), "not allowed") || strings.Contains(err.Error(), "invalid") {
			h.sendCheckErrorResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		h.logger.Error("failed to check file existence", err, map[string]any{
			"s3_key": s3Key,
		})
		h.sendCheckErrorResponse(w, http.StatusInternalServerError, "internal server error")
		return
	}

	response := CheckResponse{
		Exists:    result.Exists,
		Cached:    result.Cached,
		Size:      result.Size,
		Timestamp: result.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
		S3Key:     result.S3Key,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode check response", err, nil)
	}
}

func (h *HTTPHandler) sendCheckErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	response := ErrorResponse{
		Error: message,
	}

	if strings.Contains(message, "not allowed") {
		if h.cache != nil {
			response.AllowedPrefixes = h.cache.GetAllowedPrefixes()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode check error response", err, nil)
	}
}

func (h *HTTPHandler) ClearCacheHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.sendClearCacheErrorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var s3Key string

	path := r.URL.Path
	if strings.HasPrefix(path, "/clear-cache/") {
		s3Key = strings.TrimPrefix(path, "/clear-cache/")
	} else {
		s3Key = r.URL.Query().Get("key")
	}

	if err := h.cache.ClearCache(r.Context(), s3Key); err != nil {
		if strings.Contains(err.Error(), "not allowed") || strings.Contains(err.Error(), "invalid") {
			h.sendClearCacheErrorResponse(w, http.StatusBadRequest, err.Error())
			return
		}

		h.logger.Error("failed to clear cache", err, map[string]any{
			"s3_key": s3Key,
		})
		h.sendClearCacheErrorResponse(w, http.StatusInternalServerError, "internal server error")
		return
	}

	var message string
	if s3Key == "" {
		message = "all cache cleared successfully"
		h.logger.Info("all cache cleared via HTTP", nil)
	} else {
		message = fmt.Sprintf("cache cleared for key: %s", s3Key)
		h.logger.Info("cache cleared via HTTP", map[string]any{
			"s3_key": s3Key,
		})
	}

	response := ClearCacheResponse{
		Success: true,
		Message: message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode clear cache response", err, nil)
	}
}

func (h *HTTPHandler) sendClearCacheErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	response := ClearCacheResponse{
		Success: false,
		Error:   message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("failed to encode clear cache error response", err, nil)
	}
}
