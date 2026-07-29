package runtime

import (
	"sync"

	"github.com/local/pve-web/internal/config"
	"github.com/local/pve-web/internal/credentials"
	"github.com/local/pve-web/internal/proxmox"
)

type State struct {
	mu      sync.RWMutex
	cfg     config.Config
	creds   map[string]credentials.Credential
	clients map[string]*proxmox.Client
}

func New(cfg config.Config, creds map[string]credentials.Credential, clients map[string]*proxmox.Client) *State {
	return &State{cfg: cfg, creds: creds, clients: clients}
}

func (s *State) Snapshot() (config.Config, map[string]credentials.Credential, map[string]*proxmox.Client) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.creds, s.clients
}

func (s *State) Replace(cfg config.Config, creds map[string]credentials.Credential, clients map[string]*proxmox.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg, s.creds, s.clients = cfg, creds, clients
}
