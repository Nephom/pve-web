package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/local/pve-web/internal/cache"
	"github.com/local/pve-web/internal/config"
	"github.com/local/pve-web/internal/proxmox"
	"github.com/local/pve-web/internal/runtime"
)

type Refresher struct {
	runtime *runtime.State
	store   *cache.Store
}

func NewRefresher(runtimeState *runtime.State, store *cache.Store) *Refresher {
	return &Refresher{runtime: runtimeState, store: store}
}
func (r *Refresher) Run(ctx context.Context) {
	r.Refresh(ctx)
	cfg, _, _ := r.runtime.Snapshot()
	ticker := time.NewTicker(config.Duration(cfg.Refresh.IntervalSeconds))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Refresh(ctx)
		}
	}
}
func (r *Refresher) Refresh(parent context.Context) {
	cfg, _, clients := r.runtime.Snapshot()
	for _, t := range cfg.Targets {
		if !t.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		r.one(ctx, t, clients, cfg.Refresh)
		cancel()
	}
}
func (r *Refresher) one(ctx context.Context, t config.Target, clients map[string]*proxmox.Client, refresh config.Refresh) {
	samples := map[string][]cache.Sample{}
	if previous, ok := r.store.Get(t.ID); ok && previous.Samples != nil {
		samples = previous.Samples
	}
	v := cache.TargetState{ID: t.ID, Name: t.Name, Type: t.Type, Nodes: []proxmox.Node{}, Guests: []proxmox.Guest{}, Storages: map[string][]proxmox.Storage{}, Samples: samples}
	c := clients[t.ID]
	if c == nil {
		v.Error = "credential is not configured"
		v.ErrorKind = "configuration"
		r.store.Put(v)
		return
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		v.Error, v.ErrorKind = message(err)
		r.store.Put(v)
		return
	}
	if version, versionErr := c.Version(ctx); versionErr == nil {
		for i := range nodes { nodes[i].PVEVersion = version.Version }
	}
	guests, gerr := c.Guests(ctx)
	if gerr != nil {
		v.Error, v.ErrorKind = message(gerr)
	}
	v.Nodes = nodes
	v.Guests = guests
	if t.CephEnabled() {
		if ceph, cephErr := c.CephSummary(ctx); cephErr == nil { v.Ceph = &ceph } else if v.Error == "" { v.Error, v.ErrorKind = message(cephErr) }
	}
	if t.HAEnabled() {
		if resources, haErr := c.HAResources(ctx); haErr == nil {
			managed := map[string]bool{}
			for _, resource := range resources { managed[resource.SID] = true }
			for i := range v.Guests {
				prefix := "vm:"
				if v.Guests[i].Type == "lxc" { prefix = "ct:" }
				v.Guests[i].HAManaged = managed[prefix+fmt.Sprint(v.Guests[i].VMID)]
			}
		}
	}
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].Node < v.Nodes[j].Node })
	sort.Slice(v.Guests, func(i, j int) bool {
		if v.Guests[i].VMID == v.Guests[j].VMID {
			return v.Guests[i].Name < v.Guests[j].Name
		}
		return v.Guests[i].VMID < v.Guests[j].VMID
	})
	storageNodes := nodes
	if t.Type == "cluster" && len(storageNodes) > 1 {
		storageNodes = storageNodes[:1]
	}
	for _, n := range storageNodes {
		st, e := c.Storages(ctx, n.Node)
		if e != nil && v.Error == "" {
			v.Error, v.ErrorKind = message(e)
		} else {
			v.Storages[n.Node] = st
		}
	}
	v.LastRefresh = time.Now()
	r.store.Put(v)
	for _, n := range nodes {
		r.store.UpdateSample(t.ID, "node/"+n.Node, cache.Sample{At: time.Now(), CPU: n.CPU, Memory: percent(n.Mem, n.MaxMem)}, refresh.HistoryMinutes*60/refresh.MetricsSeconds)
	}
	for _, g := range guests {
		key := "guest/" + g.Type + "/" + fmt.Sprint(g.VMID)
		r.store.UpdateSample(t.ID, key, cache.Sample{At: time.Now(), CPU: g.CPU, Memory: percent(g.Mem, g.MaxMem)}, refresh.HistoryMinutes*60/refresh.MetricsSeconds)
	}
}
func percent(v, max int64) float64 {
	if max <= 0 {
		return 0
	}
	return float64(v) * 100 / float64(max)
}
func message(err error) (string, string) {
	var e *proxmox.APIError
	if errors.As(err, &e) {
		if e.Status == 401 {
			return e.Error(), "authentication"
		}
		if e.Status == 403 {
			return e.Error(), "permission"
		}
		if e.Kind == "connection" {
			return e.Error(), "connection"
		}
	}
	if strings.Contains(err.Error(), "credential") {
		return err.Error(), "configuration"
	}
	return err.Error(), "proxmox_api"
}
