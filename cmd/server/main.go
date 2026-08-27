package main

import (
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
	auth "github.com/isyuah/gline/internal/server"
	"github.com/isyuah/gline/internal/server/modules"
	"github.com/isyuah/gline/internal/server/sink"
)

func main() {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
		})
	})

	api := r.Group("/api/v1")
	api.Use(auth.AuthMiddleware())
	api.POST("/entries/upload", (&modules.EntriesUploadHandler{
		Sink: sink.TestSink{},
	}).HandleUploadEntries)

	if err := r.Run(":8080"); err != nil {
		fmt.Printf("Error starting server: %s\n", err)
		os.Exit(1)
	}
}
