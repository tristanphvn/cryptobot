package handlers

import (
	"net/http"
	"strconv"

	"cryptobot/internal/database/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type AuditHandler struct {
	db *gorm.DB
}

func NewAuditHandler(db *gorm.DB) *AuditHandler {
	return &AuditHandler{db: db}
}

// ListAuditLogs returns paginated audit logs with optional symbol filter.
// GET /api/v1/audit-logs?symbol=BTCUSDT&limit=50&offset=0
func (h *AuditHandler) ListAuditLogs(c *gin.Context) {
	symbol := c.Query("symbol")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	if limit > 200 {
		limit = 200
	}

	q := h.db.Model(&models.AuditLog{}).Order("created_at DESC")
	if symbol != "" {
		q = q.Where("symbol = ?", symbol)
	}

	var total int64
	q.Count(&total)

	// CRY-38: the list never ships the prompt / raw_response blobs (~1.4 KB
	// per row, polled by the dashboard). The single-entry endpoints below
	// still return them.
	var logs []models.AuditLog
	if err := q.Omit("prompt", "raw_response").Limit(limit).Offset(offset).Find(&logs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"total": total,
		"logs":  logs,
	})
}

// GetAuditLog returns a single audit log entry by ID.
// GET /api/v1/audit-logs/:id
func (h *AuditHandler) GetAuditLog(c *gin.Context) {
	id := c.Param("id")

	var entry models.AuditLog
	if err := h.db.First(&entry, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "audit log not found"})
		return
	}

	c.JSON(http.StatusOK, entry)
}

// GetTradeAuditLog returns the audit log entry linked to a specific trade.
// GET /api/v1/trades/:id/audit
func (h *AuditHandler) GetTradeAuditLog(c *gin.Context) {
	tradeID := c.Param("id")

	var entry models.AuditLog
	if err := h.db.Where("trade_id = ?", tradeID).First(&entry).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no audit log for this trade"})
		return
	}

	c.JSON(http.StatusOK, entry)
}
