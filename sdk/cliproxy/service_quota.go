package cliproxy

import (
	"context"
	"sync"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const quotaPollConcurrency = 4

type quotaPollAccount struct {
	auth   *coreauth.Auth
	policy *coreauth.InitialQuotaPolicy
	proxy  string
	next   time.Time
	cancel context.CancelFunc
}
type quotaPollResult struct {
	account *quotaPollAccount
	started time.Time
	body    []byte
	err     error
}

type quotaPoller struct {
	accounts map[string]*quotaPollAccount
	active   map[*quotaPollAccount]bool
	results  chan quotaPollResult
	workers  sync.WaitGroup
	fetch    func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error)
}

func newQuotaPoller() *quotaPoller {
	return &quotaPoller{
		accounts: make(map[string]*quotaPollAccount), active: make(map[*quotaPollAccount]bool), results: make(chan quotaPollResult, quotaPollConcurrency),
		fetch: func(ctx context.Context, cfg *config.Config, a *coreauth.Auth) ([]byte, error) {
			token, _ := a.Metadata["access_token"].(string)
			return claudeauth.NewClaudeAuthWithProxyURL(cfg, a.ProxyURL).FetchOAuthUsage(ctx, token)
		},
	}
}

func (p *quotaPoller) scan(ctx context.Context, cfg *config.Config, policy *coreauth.InitialQuotaPolicy, auths []*coreauth.Auth, now time.Time) {
	enabled := cfg != nil && !cfg.Home.Enabled && cfg.Routing.SessionAffinity && cfg.Routing.QuotaAwareInitial && policy != nil
	live := make(map[string]*coreauth.Auth, len(auths))
	if enabled {
		policy.Retain(auths)
		for _, a := range auths {
			if coreauth.ClaudeQuotaEligible(a) {
				live[a.ID] = a
			}
		}
	}
	for id, state := range p.accounts {
		a := live[id]
		if a == nil || state.policy != policy || state.auth.RegistrationEpoch != a.RegistrationEpoch || state.auth.CredentialVersion != a.CredentialVersion || state.auth.ProxyURL != a.ProxyURL || state.proxy != cfg.ProxyURL {
			if state.cancel != nil {
				state.cancel()
			}
			delete(p.accounts, id)
		}
	}
	// Iterate the supplied list rather than a map so a saturated poller is reproducible.
	for _, a := range auths {
		if live[a.ID] == nil {
			continue
		}
		state := p.accounts[a.ID]
		if state == nil {
			state = &quotaPollAccount{auth: a, policy: policy, proxy: cfg.ProxyURL}
			p.accounts[a.ID] = state
		}
		alreadyReading := false
		for active := range p.active {
			if active.auth.ID == a.ID {
				alreadyReading = true
				break
			}
		}
		if alreadyReading || state.cancel != nil || now.Before(state.next) || len(p.active) >= quotaPollConcurrency {
			continue
		}
		requestCtx, cancel := context.WithCancel(ctx)
		state.auth, state.cancel = a, cancel
		p.active[state] = true
		p.workers.Add(1)
		go func(state *quotaPollAccount) {
			defer p.workers.Done()
			body, err := p.fetch(requestCtx, cfg, state.auth)
			result := quotaPollResult{state, now, body, err}
			select {
			case p.results <- result:
			case <-ctx.Done():
			}
		}(state)
	}
}

func (p *quotaPoller) complete(result quotaPollResult, now time.Time) {
	state := result.account
	delete(p.active, state)
	state.cancel()
	state.cancel = nil
	if p.accounts[state.auth.ID] != state {
		return
	} // Removed/replaced credentials cannot publish.
	err := result.err
	if err == nil {
		err = state.policy.Observe(state.auth, result.body, result.started)
	}
	state.next = state.policy.NextPoll(state.auth, now, err == nil)
	if err != nil {
		// Do not log response bodies, token-bearing errors or credential metadata.
		log.WithField("auth_id", state.auth.ID).Debug("Claude quota read unavailable; retaining fallback routing")
	}
}

func (p *quotaPoller) stop() {
	for state := range p.active {
		state.cancel()
	}
	p.workers.Wait()
}

// startQuotaObserver performs only fixed-endpoint control-plane GETs. It starts
// even when disabled so config hot reloads can enable it without a restart.
func (s *Service) startQuotaObserver(ctx context.Context) {
	if s.coreManager == nil {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.quotaCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.quotaCancel, s.quotaDone = cancel, done
	go func() {
		defer close(done)
		poller := newQuotaPoller()
		defer poller.stop()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		scan := func() {
			poller.scan(ctx, s.currentConfig(), s.coreManager.InitialQuotaPolicy(), s.coreManager.List(), time.Now())
		}
		scan()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scan()
			case result := <-poller.results:
				// Reconcile config and credential replacements before accepting a result.
				scan()
				poller.complete(result, time.Now())
			}
		}
	}()
}

func (s *Service) stopQuotaObserver() {
	s.quotaMu.Lock()
	cancel, done := s.quotaCancel, s.quotaDone
	s.quotaCancel, s.quotaDone = nil, nil
	s.quotaMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
