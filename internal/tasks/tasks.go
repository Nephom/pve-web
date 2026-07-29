package tasks

import (
	"context"
	"fmt"
	"log"
	"github.com/local/pve-web/internal/proxmox"
	"sync"
	"time"
)

type Job struct {
	ID          string        `json:"id"`
	TargetID    string        `json:"target_id"`
	Guest       proxmox.Guest `json:"guest"`
	Action      string        `json:"action"`
	Status      string        `json:"status"`
	Message     string        `json:"message"`
	GuestStatus string        `json:"guest_status"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
}
type Manager struct {
	mu   sync.RWMutex
	jobs map[string]Job
	seq  int
}

func New() *Manager { return &Manager{jobs: map[string]Job{}} }
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.jobs[id]
	return v, ok
}
func (m *Manager) Start(ctx context.Context, targetID string, c *proxmox.Client, g proxmox.Guest, action string) (Job, error) {
	upid, err := c.GuestPower(ctx, g, action)
	if err != nil {
		return Job{}, err
	}
	log.Printf("guest power submitted target=%s type=%s vmid=%d action=%s ha_managed=%t upid=%q", targetID, g.Type, g.VMID, action, g.HAManaged, upid)
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)
	j := Job{ID: id, TargetID: targetID, Guest: g, Action: action, Status: "running", Message: "Proxmox task running", GuestStatus: g.Status, StartedAt: time.Now()}
	m.jobs[id] = j
	m.mu.Unlock()
	taskContext, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	go func() { defer cancel(); m.monitor(taskContext, id, c, g, action, upid) }()
	return j, nil
}
func (m *Manager) monitor(ctx context.Context, id string, c *proxmox.Client, g proxmox.Guest, action, upid string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	taskErrors := 0
	for {
		select {
		case <-ctx.Done():
			m.finish(id, "failed", ctx.Err().Error())
			return
		case <-ticker.C:
			if current, statusErr := c.GuestStatus(ctx, g); statusErr == nil {
				g.Status = current.Status
				m.update(id, "running", "Proxmox task running; guest status: "+current.Status, current.Status)
			}
			if upid == "" {
				if reachedDesiredStatus(g.Status, action) {
					m.finish(id, "succeeded", "guest reached requested state")
					return
				}
				taskErrors++
				if taskErrors >= 5 { m.finish(id, "failed", "Proxmox returned no task UPID and guest did not reach requested state"); return }
				m.update(id, "running", "waiting for guest state change; no task UPID returned", g.Status)
				continue
			}
			s, e := c.TaskStatus(ctx, g.Node, upid)
			if e != nil {
				taskErrors++
				if taskErrors >= 5 { m.finish(id, "failed", "task status unavailable: "+e.Error()); return }
				m.update(id, "running", e.Error(), g.Status)
				continue
			}
			taskErrors = 0
			if s.Status == "stopped" {
				if s.ExitStatus == "OK" {
					if reachedDesiredStatus(g.Status, action) { m.finish(id, "succeeded", "Proxmox task completed") } else { m.finish(id, "failed", "Proxmox task completed but guest remains "+g.Status) }
				} else {
					m.finish(id, "failed", "Proxmox task failed: "+s.ExitStatus)
				}
				return
			}
			m.update(id, "running", "Proxmox task running", g.Status)
		}
	}
}

func reachedDesiredStatus(status, action string) bool {
	switch action {
	case "start":
		return status == "running" || status == "started"
	case "shutdown", "stop":
		return status == "stopped"
	case "reboot":
		return status == "running" || status == "started"
	default:
		return false
	}
}
func (m *Manager) update(id, status, msg, guestStatus string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	j.Status = status
	j.Message = msg
	j.GuestStatus = guestStatus
	m.jobs[id] = j
}
func (m *Manager) finish(id, status, msg string) {
	m.mu.Lock()
	j := m.jobs[id]
	now := time.Now()
	j.Status = status
	j.Message = msg
	j.FinishedAt = &now
	m.jobs[id] = j
	m.mu.Unlock()
	log.Printf("guest power finished id=%s target=%s type=%s vmid=%d action=%s status=%s message=%q", id, j.TargetID, j.Guest.Type, j.Guest.VMID, j.Action, status, msg)
}
