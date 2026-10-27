// Package maintenance closes admission without cancelling work already admitted.
package maintenance

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/google/uuid"
)

var (
	ErrDraining = errors.New(`{"error":{"message":"Proxy maintenance in progress; retry later","type":"server_error","code":"proxy_draining"}}`)
	ErrOwner    = errors.New("maintenance is owned by another operation")
)

// State is an instantaneous view; Ready remains true until the owner resumes.
type State struct {
	Instance string `json:"instance"`
	Draining bool   `json:"draining"`
	Active   int    `json:"active"`
	Ready    bool   `json:"ready"`
	Durable  bool   `json:"durable"`
}

// Gate serializes admission and maintenance. Only the drain owner may reopen it.
type Gate struct {
	mu       sync.Mutex
	instance string
	owner    string
	active   int
	path     string
}

func New() *Gate { return &Gate{instance: uuid.NewString()} }

// ValidToken validates an operation identifier at the API and disk boundaries.
func ValidToken(token string) bool {
	if len(token) < 32 || len(token) > 128 {
		return false
	}
	for _, c := range []byte(token) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Open restores closed admission before a restarted process can accept work.
// A partial, public or invalid marker fails startup rather than reopening admission.
func Open(path string) (*Gate, error) {
	g := New()
	g.path = path
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() < 32 || info.Size() > 128 {
		return nil, errors.New("invalid private maintenance marker")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !ValidToken(string(data)) {
		return nil, errors.New("invalid maintenance owner")
	}
	g.owner = string(data)
	return g, nil
}

func (g *Gate) saveOwner(token string) error {
	if g.path == "" {
		return nil
	}
	if !ValidToken(token) {
		return errors.New("invalid maintenance owner")
	}
	file, err := os.OpenFile(g.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, errWrite := file.WriteString(token)
	errSync := file.Sync()
	errClose := file.Close()
	if err := errors.Join(errWrite, errSync, errClose); err != nil {
		return errors.Join(err, os.Remove(g.path))
	}
	return nil
}

func (g *Gate) state() State {
	return State{Instance: g.instance, Draining: g.owner != "", Active: g.active, Ready: g.owner != "" && g.active == 0, Durable: g.path != ""}
}

func (g *Gate) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state()
}

// Begin returns an idempotent release, or refuses work after admission closes.
func (g *Gate) Begin() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owner != "" {
		return nil, ErrDraining
	}
	g.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			g.mu.Unlock()
		})
	}, nil
}

// Drain is idempotent for its owner. Tokens must be validated at the API boundary.
func (g *Gate) Drain(token string) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if token == "" || (g.owner != "" && g.owner != token) {
		return g.state(), ErrOwner
	}
	if g.owner == "" {
		if err := g.saveOwner(token); err != nil {
			return g.state(), err
		}
	}
	g.owner = token
	return g.state(), nil
}

func (g *Gate) Resume(token string) (State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if token == "" || (g.owner != "" && g.owner != token) {
		return g.state(), ErrOwner
	}
	if g.path != "" && g.owner != "" {
		if err := os.Remove(g.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return g.state(), err
		}
	}
	g.owner = ""
	return g.state(), nil
}

type requestKey struct{}
type requestAdmission struct {
	gate *Gate
	done func()
}

// WithRequest carries admission into handlers that detach from HTTP, such as WebSockets.
func WithRequest(ctx context.Context, gate *Gate, done func()) context.Context {
	return context.WithValue(ctx, requestKey{}, requestAdmission{gate: gate, done: done})
}

// ReleaseRequest lets an idle Responses socket release its HTTP handshake ticket.
func ReleaseRequest(ctx context.Context) {
	if request, ok := ctx.Value(requestKey{}).(requestAdmission); ok {
		request.done()
	}
}

// BeginTurn admits one Responses turn, including its complete downstream delivery.
func BeginTurn(ctx context.Context) (func(), error) {
	if request, ok := ctx.Value(requestKey{}).(requestAdmission); ok {
		return request.gate.Begin()
	}
	return func() {}, nil
}

func Draining(ctx context.Context) bool {
	if request, ok := ctx.Value(requestKey{}).(requestAdmission); ok {
		return request.gate.State().Draining
	}
	return false
}
