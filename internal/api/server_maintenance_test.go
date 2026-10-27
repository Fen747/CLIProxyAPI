package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/maintenance"
)

type maintenanceStreamExecutor struct {
	codexSearchCaptureExecutor
	started chan context.Context
	chunks  chan coreexecutor.StreamChunk
}

type maintenanceLiveExecutor struct {
	codexSearchCaptureExecutor
	hangups int
}

func (e *maintenanceLiveExecutor) HttpRequest(_ context.Context, _ *auth.Auth, request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/hangup") {
		e.hangups++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"ok"}`))}, nil
	}
	return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{
		"Content-Type": {"application/sdp"}, "Location": {"/v1/realtime/calls/call-maintenance"},
	}, Body: io.NopCloser(strings.NewReader("v=0\r\n"))}, nil
}

func (e *maintenanceStreamExecutor) ExecuteStream(ctx context.Context, _ *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.started <- ctx
	return &coreexecutor.StreamResult{Chunks: e.chunks}, nil
}

const testMaintenanceToken = "0123456789abcdef0123456789abcdef"

func maintenanceCall(t *testing.T, server *Server, method, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, maintenancePath, strings.NewReader(fmt.Sprintf(`{"token":%q,"instance":%q}`, token, server.maintenance.State().Instance)))
	request.RemoteAddr = "127.0.0.1:12345"
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	server.engine.ServeHTTP(recorder, request)
	return recorder
}

func TestMaintenanceControlRequiresAuthAndOwnership(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	server := newTestServerWithOptions(t, WithLocalManagementPassword("test-management"))
	if response := maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodGet, http.MethodDelete} {
		response := maintenanceCall(t, server, method, testMaintenanceToken, "test-management")
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", method, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), testMaintenanceToken) {
			t.Fatal("operation token exposed")
		}
	}
	maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management")
	response := maintenanceCall(t, server, http.MethodDelete, strings.Repeat("x", 32), "test-management")
	if response.Code != http.StatusConflict || !server.maintenance.State().Draining {
		t.Fatal("non-owner resumed maintenance")
	}
	if response := maintenanceCall(t, server, http.MethodPut, "short", "test-management"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid token status = %d", response.Code)
	}
}

func TestMaintenanceCORSPreflightAndRejectionsRemainReadable(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	server := newTestServer(t)
	maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management")
	for _, method := range []string{http.MethodOptions, http.MethodDelete} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, maintenancePath, nil)
		request.Header.Set("Origin", "https://management.example")
		request.Header.Set("Access-Control-Request-Method", http.MethodDelete)
		server.engine.ServeHTTP(recorder, request)
		if recorder.Header().Get("Access-Control-Allow-Origin") == "" {
			t.Fatal("maintenance response lacks CORS headers")
		}
		if method == http.MethodOptions && recorder.Code != http.StatusNoContent {
			t.Fatalf("resume preflight = %d", recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("rejected inference lacks CORS headers: %d", recorder.Code)
	}
}

func TestMaintenanceRestartStaysClosedAndInvalidMarkerFailsStartup(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	server := newTestServer(t)
	if response := maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management"); response.Code != http.StatusOK {
		t.Fatalf("drain = %s", response.Body.String())
	}
	restarted := NewServer(server.cfg, nil, nil, server.configFilePath)
	if state := restarted.maintenance.State(); !state.Ready || state.Instance == server.maintenance.State().Instance {
		t.Fatalf("restored = %+v", state)
	}
	if response := maintenanceCall(t, restarted, http.MethodDelete, testMaintenanceToken, "test-management"); response.Code != http.StatusOK {
		t.Fatalf("resume = %s", response.Body.String())
	}
	if err := os.WriteFile(filepath.Join(server.cfg.AuthDir, ".maintenance.state"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := NewServer(server.cfg, nil, nil, server.configFilePath)
	if err := invalid.Start(); err == nil {
		t.Fatal("invalid marker opened a listener")
	}
	if !invalid.maintenance.State().Draining {
		t.Fatal("invalid marker reopened admission")
	}
}

func TestMaintenanceDrainsHTTPAndSSEWithoutCancelling(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			server := newTestServerWithOptions(t, WithLocalManagementPassword("test-management"))
			started, finish, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			server.engine.POST("/maintenance-test", func(c *gin.Context) {
				if streaming {
					c.Header("Content-Type", "text/event-stream")
					c.String(http.StatusOK, "data: first\n\n")
					c.Writer.Flush()
				}
				close(started)
				select {
				case <-finish:
					c.String(http.StatusOK, "complete")
				case <-c.Request.Context().Done():
					t.Error("drain cancelled admitted work")
				}
			})
			go func() {
				defer close(exited)
				server.engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/maintenance-test", nil))
			}()
			<-started
			response := maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management")
			if response.Code != http.StatusOK || server.maintenance.State().Ready || server.maintenance.State().Active != 1 {
				t.Fatalf("drain = %s, %+v", response.Body.String(), server.maintenance.State())
			}
			for _, path := range []string{"/maintenance-test", "/v8/management/requests/api-call", "/custom-plugin-path"} {
				recorder := httptest.NewRecorder()
				server.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
				if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
					t.Fatalf("new work %s = %d", path, recorder.Code)
				}
			}
			close(finish)
			<-exited
			if state := server.maintenance.State(); !state.Ready {
				t.Fatalf("after delivery = %+v", state)
			}
			maintenanceCall(t, server, http.MethodDelete, testMaintenanceToken, "test-management")
			if state := server.maintenance.State(); state.Draining {
				t.Fatal("resume failed")
			}
		})
	}
}

func TestMaintenanceIdleResponsesSocketDoesNotBlockAndCannotStartTurn(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	server := newTestServerWithOptions(t, WithLocalManagementPassword("test-management"))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
		t.Run(path, func(t *testing.T) {
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+path, http.Header{"Authorization": {"Bearer test-key"}})
			if err != nil {
				t.Fatalf("dial = %v, response=%v", err, response)
			}
			defer conn.Close()
			// A synthetic prewarm is answered locally. Receiving it proves the
			// socket handler released its handshake and completed one admitted turn.
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5","generate":false,"input":[]}`)); err != nil {
				t.Fatal(err)
			}
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(payload), "response.completed") {
					break
				}
			}
			// Drain can race the final prewarm write; repeating the authenticated
			// control call observes its eventual release without sleeping.
			for range 1000 {
				if response := maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management"); response.Code != http.StatusOK {
					t.Fatalf("drain = %d: %s", response.Code, response.Body.String())
				}
				if server.maintenance.State().Ready {
					break
				}
			}
			if !server.maintenance.State().Ready {
				t.Fatal("idle Responses socket blocks drainage")
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			_, payload, err := conn.ReadMessage()
			if err != nil || !strings.Contains(string(payload), "proxy_draining") {
				t.Fatalf("maintenance response = %s, %v", payload, err)
			}
			_, denied, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+path, nil)
			if err == nil || denied == nil || denied.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("new socket = %v, %v", denied, err)
			}
			if denied != nil {
				_, _ = io.Copy(io.Discard, denied.Body)
				_ = denied.Body.Close()
			}
			response = maintenanceCall(t, server, http.MethodGet, "", "test-management").Result()
			var status struct {
				Maintenance maintenance.State `json:"maintenance"`
			}
			if err := json.NewDecoder(response.Body).Decode(&status); err != nil || !status.Maintenance.Ready {
				t.Fatalf("status = %+v, %v", status, err)
			}
			_ = response.Body.Close()
			maintenanceCall(t, server, http.MethodDelete, testMaintenanceToken, "test-management")
		})
	}
}

func TestMaintenanceLetsActiveWebsocketTurnFinishAndRejectsNewFrames(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	exited := make(chan struct{})
	server := newTestServerWithOptions(t, WithMiddleware(func(c *gin.Context) {
		if c.FullPath() == "/v1/responses" {
			defer close(exited)
		}
		c.Next()
	}))
	executor := &maintenanceStreamExecutor{started: make(chan context.Context, 1), chunks: make(chan coreexecutor.StreamChunk, 1)}
	server.handlers.AuthManager.RegisterExecutor(executor)
	const authID, model = "maintenance-stream-auth", "maintenance-stream-model"
	if _, err := server.handlers.AuthManager.Register(context.Background(), &auth.Auth{
		ID: authID, Provider: "codex", Status: auth.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(authID)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	create := []byte(`{"type":"response.create","model":"` + model + `","input":[]}`)
	if err := conn.WriteMessage(websocket.TextMessage, create); err != nil {
		t.Fatal(err)
	}
	executionCtx := <-executor.started
	response := maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management")
	if response.Code != http.StatusOK || server.maintenance.State().Active != 1 || server.maintenance.State().Ready {
		t.Fatalf("active turn = %+v, response=%s", server.maintenance.State(), response.Body.String())
	}
	if err := conn.WriteMessage(websocket.TextMessage, create); err != nil {
		t.Fatal(err)
	}
	_, rejected, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(rejected), "proxy_draining") {
		t.Fatalf("new frame = %s, %v", rejected, err)
	}
	select {
	case <-executionCtx.Done():
		t.Fatal("maintenance cancelled active turn")
	default:
	}
	executor.chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"maintenance-result","output":[]}}`)}
	close(executor.chunks)
	_, completed, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(completed), "maintenance-result") {
		t.Fatalf("completion = %s, %v", completed, err)
	}
	_ = conn.Close()
	<-exited
	if !server.maintenance.State().Ready {
		t.Fatalf("finished turn = %+v", server.maintenance.State())
	}
}

func TestMaintenanceAllowsSidebandAndHangupForAdmittedLiveCall(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management")
	server := newTestServer(t)
	defer server.codexLiveHandler.Close()
	executor := &maintenanceLiveExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	if _, err := server.handlers.AuthManager.Register(context.Background(), &auth.Auth{
		ID: "maintenance-live-auth", Provider: "codex", Status: auth.StatusActive,
		Metadata: map[string]any{"access_token": "test-oauth-token"},
	}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader("v=0\r\n"))
		request.Header.Set("Authorization", "Bearer test-key")
		request.Header.Set("Content-Type", "application/sdp")
		server.engine.ServeHTTP(recorder, request)
		return recorder
	}
	if response := call(http.MethodPost, "/v1/realtime/calls"); response.Code != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", response.Code, response.Body.String())
	}
	if server.codexLiveHandler.ActiveSessions() != 1 {
		t.Fatal("bootstrap did not retain admitted call")
	}
	response := maintenanceCall(t, server, http.MethodPut, testMaintenanceToken, "test-management")
	if !strings.Contains(response.Body.String(), `"ready":false`) {
		t.Fatalf("retained call was omitted: %s", response.Body.String())
	}
	for _, path := range []string{"/v1/live/call-maintenance", "/v1/realtime/calls/call-maintenance", "/v1/realtime?call_id=call-maintenance"} {
		if response := call(http.MethodGet, path); response.Code != http.StatusUpgradeRequired {
			t.Fatalf("existing sideband %s = %d: %s", path, response.Code, response.Body.String())
		}
	}
	if response := call(http.MethodPost, "/v1/realtime/calls/call-unknown/hangup"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown call admitted: %d", response.Code)
	}
	if response := call(http.MethodPost, "/v1/realtime/calls/call-maintenance/hangup"); response.Code != http.StatusOK {
		t.Fatalf("existing hangup = %d: %s", response.Code, response.Body.String())
	}
	response = maintenanceCall(t, server, http.MethodGet, "", "test-management")
	if executor.hangups != 1 || !strings.Contains(response.Body.String(), `"ready":true`) {
		t.Fatalf("hangup did not complete drainage: %s", response.Body.String())
	}
}
