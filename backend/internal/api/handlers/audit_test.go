package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"cryptobot/internal/database/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// CRY-38: the list endpoint omits the prompt / raw_response blobs; the
// single-entry endpoint still returns them.
func TestAuditLogs_ListOmitsTextDetailKeepsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.AuditLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	entry := models.AuditLog{Symbol: "BTCUSDT", Interval: "1h", Prompt: "the prompt", RawResponse: `{"action":"HOLD"}`,
		Action: "HOLD", Reasoning: "flat", PromptSentAt: time.Now(), ResponseAt: time.Now()}
	db.Create(&entry)

	h := NewAuditHandler(db)
	r := gin.New()
	r.GET("/audit-logs", h.ListAuditLogs)
	r.GET("/audit-logs/:id", h.GetAuditLog)

	get := func(path string) map[string]any {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		return m
	}

	logs := get("/audit-logs")["logs"].([]any)
	if len(logs) != 1 {
		t.Fatalf("logs=%d want 1", len(logs))
	}
	row := logs[0].(map[string]any)
	if row["prompt"] != "" || row["raw_response"] != "" {
		t.Fatalf("list leaked text: prompt=%q raw_response=%q", row["prompt"], row["raw_response"])
	}
	if row["reasoning"] != "flat" || row["action"] != "HOLD" {
		t.Fatalf("list must keep structured fields: %+v", row)
	}

	detail := get("/audit-logs/" + strconv.Itoa(int(entry.ID)))
	if detail["prompt"] != "the prompt" || detail["raw_response"] != `{"action":"HOLD"}` {
		t.Fatalf("detail must return text: %+v", detail)
	}
}
