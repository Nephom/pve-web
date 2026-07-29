package cache

import (
	"github.com/local/pve-web/internal/proxmox"
	"sort"
	"sync"
	"time"
)

type Sample struct {
	At     time.Time `json:"at"`
	CPU    float64   `json:"cpu"`
	Memory float64   `json:"memory"`
}
type TargetState struct {
	ID          string                       `json:"id"`
	Name        string                       `json:"name"`
	Type        string                       `json:"type"`
	Nodes       []proxmox.Node               `json:"nodes"`
	Guests      []proxmox.Guest              `json:"guests"`
	Storages    map[string][]proxmox.Storage `json:"storages"`
	Error       string                       `json:"error,omitempty"`
	ErrorKind   string                       `json:"error_kind,omitempty"`
	LastRefresh time.Time                    `json:"last_refresh"`
	Samples     map[string][]Sample          `json:"samples"`
	Ceph        *proxmox.CephSummary         `json:"ceph,omitempty"`
}
type Store struct {
	mu     sync.RWMutex
	states map[string]TargetState
}

func New() *Store { return &Store{states: map[string]TargetState{}} }
func (s *Store) Get(id string) (TargetState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.states[id]
	if ok {
		v.Samples = CloneSamples(v.Samples)
	}
	return v, ok
}
func (s *Store) Put(v TargetState) { s.mu.Lock(); defer s.mu.Unlock(); s.states[v.ID] = v }
func (s *Store) UpdateSample(id, key string, sample Sample, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.states[id]
	if v.Samples == nil {
		v.Samples = map[string][]Sample{}
	}
	v.Samples[key] = append(v.Samples[key], sample)
	if len(v.Samples[key]) > limit {
		v.Samples[key] = v.Samples[key][len(v.Samples[key])-limit:]
	}
	s.states[id] = v
}

func CloneSamples(source map[string][]Sample) map[string][]Sample {
	cloned := make(map[string][]Sample, len(source))
	for key, samples := range source {
		cloned[key] = append([]Sample(nil), samples...)
	}
	return cloned
}
func Sort(v *TargetState) {
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].Node < v.Nodes[j].Node })
	sort.Slice(v.Guests, func(i, j int) bool {
		if v.Guests[i].VMID == v.Guests[j].VMID {
			return v.Guests[i].Name < v.Guests[j].Name
		}
		return v.Guests[i].VMID < v.Guests[j].VMID
	})
}
