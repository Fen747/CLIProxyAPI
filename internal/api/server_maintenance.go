package api

import (
	"errors"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/maintenance"
)

const maintenancePath = "/v8/management/server/maintenance"

func (s *Server) maintenanceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		// Keep only local controls and health available. Arbitrary management API
		// calls and plugin routes can execute upstream work and must also drain.
		if path == maintenancePath || ((c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) &&
			(path == "/healthz" || path == "/management.html" || path == "/v8/management/config")) {
			c.Next()
			return
		}
		done, err := s.maintenance.BeginExisting(func() bool { return s.existingLiveControl(c) })
		if err != nil {
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
				"message": "Proxy maintenance in progress; retry later", "type": "server_error", "code": "proxy_draining",
			}})
			return
		}
		defer done()
		c.Request = c.Request.WithContext(maintenance.WithRequest(c.Request.Context(), s.maintenance, done))
		c.Next()
	}
}

func (s *Server) existingLiveControl(c *gin.Context) bool {
	callID := c.Param("call_id")
	switch c.FullPath() {
	case "/v1/live/:call_id", "/v1/realtime/calls/:call_id":
		if c.Request.Method != http.MethodGet {
			return false
		}
	case "/v1/realtime":
		if c.Request.Method != http.MethodGet {
			return false
		}
		callID = c.Query("call_id")
	case "/v1/realtime/calls/:call_id/hangup":
		if c.Request.Method != http.MethodPost {
			return false
		}
	default:
		return false
	}
	return s.codexLiveHandler.HasSession(callID)
}

func (s *Server) maintenanceControl(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodGet {
		s.maintenanceState(c)
		return
	}
	var request struct {
		Token    string `json:"token"`
		Instance string `json:"instance"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256)
	if err := c.ShouldBindJSON(&request); err != nil || !maintenance.ValidToken(request.Token) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A 32-128 character operation token is required"})
		return
	}
	if request.Instance != s.maintenance.State().Instance {
		c.JSON(http.StatusConflict, gin.H{"error": "Proxy instance changed; read maintenance state again"})
		return
	}
	var err error
	if c.Request.Method == http.MethodDelete {
		_, err = s.maintenance.Resume(request.Token)
	} else {
		if cfg := s.getConfig(); cfg != nil && cfg.Home.Enabled {
			c.JSON(http.StatusConflict, gin.H{"error": "Home-dispatched work is not covered by local maintenance"})
			return
		}
		_, err = s.maintenance.Drain(request.Token)
	}
	if err != nil {
		if errors.Is(err, maintenance.ErrOwner) {
			c.JSON(http.StatusConflict, gin.H{"error": "Maintenance is owned by another operation"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not persist maintenance admission state"})
		}
		return
	}
	s.maintenanceState(c)
}

func (s *Server) maintenanceState(c *gin.Context) {
	cfg := s.getConfig()
	state := s.maintenance.StateWithActive(s.codexLiveHandler.ActiveSessions)
	c.JSON(http.StatusOK, gin.H{"maintenance": state, "pid": os.Getpid(), "supported": state.Durable && (cfg == nil || !cfg.Home.Enabled)})
}
