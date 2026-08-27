package modules

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/isyuah/gline/internal/logentry"
	"github.com/isyuah/gline/internal/server/sink"
)

type UploadEntriesRequest struct {
	Entries []logentry.LogEntry `json:"entries"`
}

type EntriesUploadHandler struct {
	Sink sink.EntrySink
}

func (h *EntriesUploadHandler) HandleUploadEntries(c *gin.Context) {
	var req UploadEntriesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := h.Sink.Accept(c.Request.Context(), req.Entries); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "accept entries failed",
		})
		return
	}
	c.Status(http.StatusOK)
}
