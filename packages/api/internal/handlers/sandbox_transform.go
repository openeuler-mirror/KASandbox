package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// PostSandboxesTransform answers 501 until the next patch adds the handler;
// the regenerated server interface already has the operation.
func (a *APIStore) PostSandboxesTransform(c *gin.Context) {
	c.Status(http.StatusNotImplemented)
}
