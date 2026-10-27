package cliproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type windowPrimingJob struct {
	auth      *coreauth.Auth
	policy    *coreauth.InitialQuotaPolicy
	proxy     string
	cancel    context.CancelFunc
	cancelled bool
	done      func()
}
type windowPrimingResult struct {
	job      *windowPrimingJob
	body     []byte
	observed time.Time
	err      error
}

type quotaPrimer struct {
	authDir string
	now     func() time.Time
	manager *coreauth.Manager
	path    string
	state   *primingState
	failed  bool
	active  *windowPrimingJob
	results chan windowPrimingResult
	workers sync.WaitGroup
	begin   func() (func(), error)
	run     func(context.Context, *coreauth.Auth) error
	fetch   func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error)
}

func newQuotaPrimer(manager *coreauth.Manager, begin func() (func(), error), fetch func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error)) *quotaPrimer {
	return &quotaPrimer{now: time.Now, manager: manager, results: make(chan windowPrimingResult, 1), begin: begin, run: manager.PrimeClaudeWindow, fetch: fetch}
}

func primingEnabled(cfg *config.Config) bool {
	return cfg != nil && !cfg.Home.Enabled && cfg.Routing.SessionAffinity && cfg.Routing.QuotaAwareInitial && cfg.Routing.ClaudeWindowPriming && cfg.AuthDir != ""
}

// primingSubscriptionKey survives token rotation and deduplicates duplicate auth
// files for the same subscription. Neither tokens nor account identifiers are persisted.
func primingSubscriptionKey(a *coreauth.Auth) string {
	identity := a.ID
	if id := helps.ClaudeCredentialAccountUUID(a); id != "" {
		identity = id
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("claude:"+identity)))
}

func (p *quotaPrimer) reconcile(cfg *config.Config, auths []*coreauth.Auth) {
	enabled := primingEnabled(cfg) && (p.authDir == "" || filepath.Clean(cfg.AuthDir) == filepath.Clean(p.authDir))
	path := ""
	if enabled {
		path = filepath.Join(cfg.AuthDir, ".claude-window-priming.state")
	}
	if p.active != nil {
		valid := enabled && path == p.path && cfg.ProxyURL == p.active.proxy
		var current *coreauth.Auth
		for _, a := range auths {
			if a.ID == p.active.auth.ID {
				current = a
				break
			}
		}
		if !valid || !samePrimingCredential(current, p.active.auth) {
			p.active.cancelled = true
			p.active.cancel()
		}
	}
	if path != p.path {
		if p.state != nil {
			if err := p.state.Close(); err != nil {
				log.Warn("could not close Claude priming state")
			}
		}
		p.path, p.state, p.failed = path, nil, false
	}
	if path != "" && p.state == nil && !p.failed {
		var err error
		p.state, err = openPrimingState(path)
		if err != nil {
			p.failed = true
			log.Warn("Claude window priming disabled: private state unavailable")
		}
	}
}

func samePrimingCredential(a, b *coreauth.Auth) bool {
	return coreauth.ClaudeQuotaEligible(a) && a.ID == b.ID && a.RegistrationEpoch == b.RegistrationEpoch && a.CredentialVersion == b.CredentialVersion && a.ProxyURL == b.ProxyURL
}

type primingOutcome int

const (
	primingSkipped primingOutcome = iota
	primingStarted
	primingDeferred
)

// observe runs only after a fresh successful usage read, never from a quota guess.
func (p *quotaPrimer) observe(ctx context.Context, cfg *config.Config, policy *coreauth.InitialQuotaPolicy, a *coreauth.Auth, now time.Time) primingOutcome {
	if !primingEnabled(cfg) || p.state == nil || p.failed || policy == nil {
		return primingSkipped
	}
	current, ok := p.manager.GetByID(a.ID)
	if !ok || !samePrimingCredential(current, a) {
		return primingSkipped
	}
	a = current
	canPrime, reset := policy.WindowPrimingDecision(a, now)
	key := primingSubscriptionKey(a)
	if !reset.IsZero() {
		if err := p.state.Confirm(key, reset); err != nil {
			p.failed = true
			log.Warn("Claude priming confirmation could not be persisted")
		}
		return primingSkipped
	}
	if !canPrime {
		return primingSkipped
	}
	if p.active != nil {
		return primingDeferred
	}
	done, err := p.begin()
	if err != nil {
		return primingDeferred
	}
	reserved, err := p.state.Reserve(key, now)
	if err != nil || !reserved {
		done()
		if err != nil {
			p.failed = true
			log.Warn("Claude priming attempt could not be persisted")
		}
		return primingSkipped
	}
	requestCtx, cancel := context.WithCancel(ctx)
	job := &windowPrimingJob{auth: current, policy: policy, proxy: cfg.ProxyURL, cancel: cancel, done: done}
	p.active = job
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		errRun := p.run(requestCtx, current)
		observed := p.now()
		// Even a failed response may have consumed quota; confirm before any later attempt.
		body, errRead := p.fetch(requestCtx, cfg, current)
		if errRead != nil {
			body = nil
		}
		result := windowPrimingResult{job: job, body: body, observed: observed, err: errRun}
		select {
		case p.results <- result:
		case <-ctx.Done():
		}
	}()
	return primingStarted
}

func (p *quotaPrimer) complete(result windowPrimingResult, now time.Time) bool {
	job := result.job
	if p.active != job {
		return false
	}
	p.active = nil
	defer job.done()
	job.cancel()
	if job.cancelled || p.state == nil || p.failed || len(result.body) == 0 {
		return false
	}
	if err := job.policy.Observe(job.auth, result.body, result.observed); err != nil {
		return false
	}
	_, reset := job.policy.WindowPrimingDecision(job.auth, now)
	confirmed := !reset.IsZero()
	if confirmed {
		if err := p.state.Confirm(primingSubscriptionKey(job.auth), reset); err != nil {
			p.failed = true
			return false
		}
	}
	log.WithFields(log.Fields{"auth_id": job.auth.ID, "confirmed": confirmed, "inference_succeeded": result.err == nil}).Info("Claude window priming attempt finished")
	return confirmed
}

func (p *quotaPrimer) stop() {
	if p.active != nil {
		p.active.cancel()
	}
	p.workers.Wait()
	if p.active != nil {
		p.active.done()
		p.active = nil
	}
	if p.state != nil {
		if err := p.state.Close(); err != nil {
			log.Warn("could not close Claude priming state")
		}
	}
}
