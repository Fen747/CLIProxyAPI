package auth

import (
	"container/list"
	"time"
)

type sessionBindingState struct {
	AuthID    string    `json:"auth_id"`
	Aliases   []string  `json:"aliases"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *SessionCache) snapshot() []sessionBindingState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []sessionBindingState
	now := time.Now()
	for element := c.evictionOrder.Front(); element != nil; element = element.Next() {
		g := c.groups[element.Value.(string)]
		if now.Before(g.expiresAt) {
			result = append(result, sessionBindingState{g.authID, append([]string(nil), g.aliases...), g.expiresAt})
		}
	}
	return result
}

func (c *SessionCache) restore(groups []sessionBindingState, acceptAuth func(string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]sessionEntry)
	c.groups = make(map[string]sessionEntry)
	c.evictionOrder = list.New()
	c.evictionElements = make(map[string]*list.Element)
	now := time.Now()
	for _, g := range groups {
		if !now.Before(g.ExpiresAt) || !acceptAuth(g.AuthID) || len(g.Aliases) == 0 || len(g.Aliases) > maxStableSessionAliases+1 {
			continue
		}
		valid := true
		for _, alias := range g.Aliases {
			if alias == "" || len(alias) > 8192 {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		aliases := compactSessionAliases(mergeSessionAliases(nil, g.Aliases...))
		var previous []sessionEntry
		for _, alias := range aliases {
			if entry, ok := c.entries[alias]; ok {
				previous = append(previous, entry)
			}
		}
		c.replaceAliasGroupsLocked(g.AuthID, g.ExpiresAt, aliases, previous...)
	}
}
