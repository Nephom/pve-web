package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/local/pve-web/internal/cache"
	"github.com/local/pve-web/internal/config"
	"github.com/local/pve-web/internal/credentials"
	"github.com/local/pve-web/internal/proxmox"
	"github.com/local/pve-web/internal/runtime"
	"github.com/local/pve-web/internal/tasks"
	"gopkg.in/yaml.v3"
)

type Server struct {
	runtime         *runtime.State
	store           *cache.Store
	jobs            *tasks.Manager
	configPath      string
	credentialsPath string
	sessions        map[string]consoleSession
	guestSessions   map[string]guestConsoleSession
	mu              sync.RWMutex
}
type consoleSession struct {
	Client  *proxmox.Client
	Node    string
	Term    proxmox.NodeTerminal
	Expires time.Time
}
type guestConsoleSession struct {
	Client  *proxmox.Client
	Node    string
	VNC     proxmox.GuestVNC
	Expires time.Time
}

func New(runtimeState *runtime.State, store *cache.Store, jobs *tasks.Manager, configPath, credentialsPath string) *Server {
	return &Server{runtime: runtimeState, store: store, jobs: jobs, configPath: configPath, credentialsPath: credentialsPath, sessions: map[string]consoleSession{}, guestSessions: map[string]guestConsoleSession{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	cfg, _, _ := s.runtime.Snapshot()
	base := strings.TrimRight(cfg.Server.BasePath, "/")
	mux.HandleFunc(base+"/health", s.health)
	mux.HandleFunc(base+"/version", s.version)
	mux.HandleFunc(base+"/data/overview", s.overview)
	mux.HandleFunc(base+"/data/targets/", s.target)
	mux.HandleFunc(base+"/operation/guests/", s.power)
	mux.HandleFunc(base+"/tasks/", s.job)
	mux.HandleFunc(base+"/certificates", s.certificates)
	mux.HandleFunc(base+"/config/targets", s.targets)
	mux.HandleFunc(base+"/config/targets/", s.targetConfig)
	mux.HandleFunc(base+"/console/nodes/", s.console)
	mux.HandleFunc(base+"/console/guests/", s.guestConsole)
	mux.Handle(base+"/", http.StripPrefix(base, http.FileServer(http.Dir(cfg.Server.StaticDir))))
	return withJSONHeaders(mux)
}

func withJSONHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"name": "pve-web", "version": Version, "commit": Commit, "build_time": BuildTime})
}

func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	cfg, _, clients := s.runtime.Snapshot()
	rest := strings.TrimPrefix(r.URL.Path, strings.TrimRight(cfg.Server.BasePath, "/")+"/console/nodes/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target and node required"})
		return
	}
	if len(parts) == 3 && parts[2] == "ws" {
		s.consoleWebsocket(w, r, parts[1])
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	targetID, node := parts[0], parts[1]
	client := clients[targetID]
	if client == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "target client not found"})
		return
	}
	log.Printf("node console start target=%s node=%s", targetID, node)
	term, err := client.NodeTermProxy(r.Context(), node)
	if err != nil {
		log.Printf("node console start failed target=%s node=%s error=%v", targetID, node, err)
		writeAPIError(w, err)
		return
	}
	log.Printf("node console session target=%s node=%s port=%d ticket_present=%t ticket_len=%d", targetID, node, term.Port, term.Ticket != "", len(term.Ticket))
	id := makeSessionID()
	s.mu.Lock()
	s.sessions[id] = consoleSession{Client: client, Node: node, Term: term, Expires: time.Now().Add(2 * time.Minute)}
	s.mu.Unlock()
	// user/ticket are consumed client-side to authenticate the termproxy
	// handshake line ("user:ticket\n") over the websocket; the same
	// sensitivity as the previous noVNC password already returned here.
	writeJSON(w, http.StatusCreated, map[string]any{"session_id": id, "node": node, "user": term.User, "ticket": term.Ticket, "websocket_path": strings.TrimRight(cfg.Server.BasePath, "/") + "/console/nodes/" + urlPathEscape(targetID) + "/" + urlPathEscape(node) + "/ws?session=" + id})
}

func (s *Server) consoleWebsocket(w http.ResponseWriter, r *http.Request, node string) {
	sessionID := r.URL.Query().Get("session")
	s.mu.RLock()
	session, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if !ok || session.Node != node || time.Now().After(session.Expires) {
		http.Error(w, "console session expired", http.StatusGone)
		return
	}
	local, err := consoleUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("node console websocket upgrade failed node=%s error=%v", node, err)
		return
	}
	defer local.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	remote, err := session.Client.DialNodeTermProxy(ctx, session.Node, session.Term)
	if err != nil {
		log.Printf("node console websocket failed node=%s error=%v", session.Node, err)
		_ = local.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
		return
	}
	defer remote.Close()
	relayWebsocket(local, remote)
}

func (s *Server) guestConsole(w http.ResponseWriter, r *http.Request) {
	cfg, _, clients := s.runtime.Snapshot()
	rest := strings.TrimPrefix(r.URL.Path, strings.TrimRight(cfg.Server.BasePath, "/")+"/console/guests/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target, node, guest type and vmid required"})
		return
	}
	if len(parts) == 5 && parts[4] == "ws" {
		s.guestConsoleWebsocket(w, r, parts[1], parts[2], parts[3])
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	targetID, node, guestType, vmidStr := parts[0], parts[1], parts[2], parts[3]
	if guestType != "qemu" && guestType != "lxc" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "guest type must be qemu or lxc"})
		return
	}
	vmid, err := strconv.ParseInt(vmidStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid vmid"})
		return
	}
	client := clients[targetID]
	if client == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "target client not found"})
		return
	}
	log.Printf("guest console start target=%s node=%s type=%s vmid=%d", targetID, node, guestType, vmid)
	vnc, err := client.GuestVNCProxy(r.Context(), node, guestType, vmid, 1280, 800)
	if err != nil {
		log.Printf("guest console start failed target=%s node=%s type=%s vmid=%d error=%v", targetID, node, guestType, vmid, err)
		writeAPIError(w, err)
		return
	}
	log.Printf("guest console session target=%s node=%s type=%s vmid=%d port=%d ticket_present=%t ticket_len=%d", targetID, node, guestType, vmid, vnc.Port, vnc.Ticket != "", len(vnc.Ticket))
	id := makeSessionID()
	s.mu.Lock()
	s.guestSessions[id] = guestConsoleSession{Client: client, Node: node, VNC: vnc, Expires: time.Now().Add(2 * time.Minute)}
	s.mu.Unlock()
	password := vnc.Password
	if password == "" {
		password = vnc.Ticket
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session_id": id, "node": node, "type": guestType, "vmid": vmid, "password": password, "websocket_path": strings.TrimRight(cfg.Server.BasePath, "/") + "/console/guests/" + urlPathEscape(targetID) + "/" + urlPathEscape(node) + "/" + urlPathEscape(guestType) + "/" + urlPathEscape(vmidStr) + "/ws?session=" + id})
}

func (s *Server) guestConsoleWebsocket(w http.ResponseWriter, r *http.Request, node, guestType, vmid string) {
	sessionID := r.URL.Query().Get("session")
	s.mu.RLock()
	session, ok := s.guestSessions[sessionID]
	s.mu.RUnlock()
	if !ok || session.Node != node || time.Now().After(session.Expires) {
		http.Error(w, "console session expired", http.StatusGone)
		return
	}
	local, err := consoleUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("guest console websocket upgrade failed node=%s type=%s vmid=%s error=%v", node, guestType, vmid, err)
		return
	}
	defer local.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	remote, err := session.Client.DialGuestVNC(ctx, session.Node, session.VNC)
	if err != nil {
		log.Printf("guest console websocket failed node=%s type=%s vmid=%s error=%v", session.Node, guestType, vmid, err)
		_ = local.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
		return
	}
	defer remote.Close()
	relayWebsocket(local, remote)
}

var consoleUpgrader = websocket.Upgrader{CheckOrigin: func(req *http.Request) bool {
	return req.Header.Get("Origin") == "" || strings.Contains(req.Header.Get("Origin"), req.Host)
}}

// relayWebsocket copies frames bidirectionally between the browser-facing
// websocket and the websocket dialed to the Proxmox VE vncwebsocket endpoint.
// It is protocol-agnostic: both the termproxy (xterm.js) multiplexed byte
// stream and the raw RFB/VNC byte stream are relayed transparently.
func relayWebsocket(local, remote *websocket.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		for {
			typ, data, readErr := local.ReadMessage()
			if readErr != nil {
				break
			}
			if writeErr := remote.WriteMessage(typ, data); writeErr != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	go func() {
		for {
			typ, data, readErr := remote.ReadMessage()
			if readErr != nil {
				break
			}
			if writeErr := local.WriteMessage(typ, data); writeErr != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
}

func makeSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("console-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
func urlPathEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "/", "%2F"), "?", "%3F")
}

type targetRequest struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Enabled         bool     `json:"enabled"`
	VerifyTLS       bool     `json:"verify_tls"`
	DetectHA        *bool    `json:"detect_ha"`
	DetectCeph      *bool    `json:"detect_ceph"`
	Endpoints       []string `json:"endpoints"`
	User            string   `json:"user"`
	TokenName       string   `json:"token_name"`
	TokenValue      string   `json:"token_value"`
	ConsoleUser     string   `json:"console_user"`
	ConsolePassword string   `json:"console_password"`
}

func (s *Server) targets(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, creds, _ := s.runtime.Snapshot()
		out := make([]map[string]any, 0, len(cfg.Targets))
		for _, t := range cfg.Targets {
			c := creds[t.ID]
			out = append(out, map[string]any{"id": t.ID, "name": t.Name, "type": t.Type, "enabled": t.Enabled, "verify_tls": t.VerifyTLS, "detect_ha": t.DetectHA, "detect_ceph": t.DetectCeph, "endpoints": t.Endpoints, "user": c.User, "token_name": c.TokenName, "credential_configured": c.TokenValue != "", "console_user": c.ConsoleUser, "console_configured": c.ConsolePassword != ""})
		}
		writeJSON(w, http.StatusOK, map[string]any{"targets": out})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.saveTarget(w, r, "")
}

func (s *Server) targetConfig(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	if id == "" || id == "." || id == "/" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target id required"})
		return
	}
	if r.Method == http.MethodDelete {
		cfg, creds, _ := s.runtime.Snapshot()
		found := false
		out := cfg.Targets[:0]
		for _, t := range cfg.Targets {
			if t.ID == id {
				found = true
				continue
			}
			out = append(out, t)
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "target not found"})
			return
		}
		delete(creds, id)
		cfg.Targets = out
		if err := s.applyConfig(cfg, creds); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.saveTarget(w, r, id)
}

func (s *Server) saveTarget(w http.ResponseWriter, r *http.Request, existingID string) {
	var req targetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if existingID != "" {
		req.ID = existingID
	}
	if req.ID == "" || req.Name == "" || req.Type == "" || len(req.Endpoints) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id, name, type and at least one endpoint are required"})
		return
	}
	cfg, creds, _ := s.runtime.Snapshot()
	if existingID == "" {
		for _, t := range cfg.Targets {
			if t.ID == req.ID {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "target already exists"})
				return
			}
		}
	}
	old := creds[req.ID]
	if req.User == "" {
		req.User = old.User
	}
	if req.TokenName == "" {
		req.TokenName = old.TokenName
	}
	if req.TokenValue == "" {
		req.TokenValue = old.TokenValue
	}
	if req.ConsoleUser == "" {
		req.ConsoleUser = old.ConsoleUser
		if req.ConsoleUser == "" {
			req.ConsoleUser = "root@pam"
		}
	}
	if req.ConsolePassword == "" {
		req.ConsolePassword = old.ConsolePassword
	}
	if req.User == "" || req.TokenName == "" || req.TokenValue == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user, token_name and token_value are required for a new credential"})
		return
	}
	if req.ConsoleUser == "" || req.ConsolePassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "console_user and console_password are required for Node Shell"})
		return
	}
	target := config.Target{ID: req.ID, Name: req.Name, Type: req.Type, Enabled: req.Enabled, VerifyTLS: req.VerifyTLS, DetectHA: req.DetectHA, DetectCeph: req.DetectCeph, Endpoints: req.Endpoints}
	found := false
	for i := range cfg.Targets {
		if cfg.Targets[i].ID == req.ID {
			cfg.Targets[i] = target
			found = true
		}
	}
	if !found {
		cfg.Targets = append(cfg.Targets, target)
	}
	creds[req.ID] = credentials.Credential{User: req.User, TokenName: req.TokenName, TokenValue: req.TokenValue, ConsoleUser: req.ConsoleUser, ConsolePassword: req.ConsolePassword}
	if err := cfg.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.applyConfig(cfg, creds); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "id": req.ID})
}

func (s *Server) applyConfig(cfg config.Config, creds map[string]credentials.Credential) error {
	configData, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	bundle := credentials.Bundle{Version: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Targets: make([]credentials.BundleTarget, 0, len(cfg.Targets))}
	clients := map[string]*proxmox.Client{}
	for _, t := range cfg.Targets {
		c := creds[t.ID]
		bundle.Targets = append(bundle.Targets, credentials.BundleTarget{ID: t.ID, Name: t.Name, Type: t.Type, Enabled: t.Enabled, VerifyTLS: t.VerifyTLS, DetectHA: t.DetectHA, DetectCeph: t.DetectCeph, Endpoints: t.Endpoints, Credential: c})
		clients[t.ID] = proxmox.NewClientWithConsole(proxmox.Target{ID: t.ID, Endpoints: t.Endpoints, VerifyTLS: t.VerifyTLS}, c.User, c.TokenName, c.TokenValue, c.ConsoleUser, c.ConsolePassword)
	}
	credentialData, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(s.configPath, configData, 0600); err != nil {
		return err
	}
	if err := atomicWrite(s.credentialsPath, credentialData, 0600); err != nil {
		return err
	}
	s.runtime.Replace(cfg, creds, clients)
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), ".pve-web-write.tmp")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

var Version = "dev"
var Commit = "unknown"
var BuildTime = "unknown"

func (s *Server) overview(w http.ResponseWriter, _ *http.Request) {
	cfg, _, _ := s.runtime.Snapshot()
	out := make([]cache.TargetState, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		if v, ok := s.store.Get(t.ID); ok {
			out = append(out, v)
		} else {
			out = append(out, cache.TargetState{ID: t.ID, Name: t.Name, Type: t.Type, Nodes: []proxmox.Node{}, Guests: []proxmox.Guest{}, Storages: map[string][]proxmox.Storage{}, Samples: map[string][]cache.Sample{}, Error: "not refreshed yet", ErrorKind: "unavailable"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out, "generated_at": time.Now()})
}
func (s *Server) target(w http.ResponseWriter, r *http.Request) {
	cfg, _, _ := s.runtime.Snapshot()
	id := strings.Split(strings.TrimPrefix(r.URL.Path, strings.TrimRight(cfg.Server.BasePath, "/")+"/data/targets/"), "/")[0]
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "target id required"})
		return
	}
	v, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "target not found"})
		return
	}
	writeJSON(w, 200, v)
}

type powerRequest struct {
	Action string `json:"action"`
}

func (s *Server) power(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	cfg, _, clients := s.runtime.Snapshot()
	base := strings.TrimRight(cfg.Server.BasePath, "/") + "/operation/guests/"
	bits := strings.Split(strings.TrimPrefix(r.URL.Path, base), "/")
	if len(bits) != 3 {
		writeJSON(w, 400, map[string]string{"error": "expected target/type/vmid"})
		return
	}
	targetID, typ, vmid := bits[0], bits[1], bits[2]
	var req powerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	v, ok := s.store.Get(targetID)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "target not found"})
		return
	}
	var guest *proxmox.Guest
	for i := range v.Guests {
		if v.Guests[i].Type == typ && fmt.Sprint(v.Guests[i].VMID) == vmid {
			guest = &v.Guests[i]
			break
		}
	}
	if guest == nil {
		writeJSON(w, 404, map[string]string{"error": "guest not found"})
		return
	}
	if !allowed(*guest, req.Action) {
		writeJSON(w, 409, map[string]string{"error": "action is not allowed for current guest status"})
		return
	}
	c := clients[targetID]
	if c == nil {
		writeJSON(w, 503, map[string]string{"error": "target credential is not configured"})
		return
	}
	log.Printf("guest power target=%s type=%s vmid=%s node=%s status=%s ha_managed=%t action=%s", targetID, typ, vmid, guest.Node, guest.Status, guest.HAManaged, req.Action)
	j, err := s.jobs.Start(r.Context(), targetID, c, *guest, req.Action)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, 202, j)
}
func allowed(g proxmox.Guest, a string) bool {
	switch g.Status {
	case "running", "started":
		return a == "shutdown" || a == "stop" || a == "reboot"
	case "stopped":
		return a == "start"
	}
	return false
}
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id := path.Base(r.URL.Path)
	j, ok := s.jobs.Get(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, 200, j)
}

type certificateRequest struct {
	CommonName   string   `json:"common_name"`
	DNSNames     []string `json:"dns_names"`
	IPAddresses  []string `json:"ip_addresses"`
	ValidityDays int      `json:"validity_days"`
}

type certificateMetadata struct {
	CommonName   string    `json:"common_name"`
	DNSNames     []string  `json:"dns_names"`
	IPAddresses  []string  `json:"ip_addresses"`
	ValidityDays int       `json:"validity_days"`
	GeneratedAt  time.Time `json:"generated_at"`
	CertFile     string    `json:"cert_file"`
	KeyFile      string    `json:"key_file"`
}

const maxCertificateValidityDays = 3650

func (s *Server) certificates(w http.ResponseWriter, r *http.Request) {
	cfg, _, _ := s.runtime.Snapshot()
	if r.Method == http.MethodGet {
		download := r.URL.Query().Get("download")
		filePath, fileName := "", ""
		switch download {
		case "cert":
			filePath, fileName = cfg.Server.HTTPS.CertFile, "pve-web.crt"
		case "key":
			filePath, fileName = cfg.Server.HTTPS.KeyFile, "pve-web.key"
		}
		if filePath != "" {
			file, err := os.Open(filePath)
			if err != nil {
				if os.IsNotExist(err) {
					writeJSON(w, http.StatusNotFound, map[string]string{"error": "certificate file does not exist; generate it first"})
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to open certificate file"})
				return
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to inspect certificate file"})
				return
			}
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
			http.ServeContent(w, r, fileName, info.ModTime(), file)
			return
		}
		_, certErr := os.Stat(cfg.Server.HTTPS.CertFile)
		_, keyErr := os.Stat(cfg.Server.HTTPS.KeyFile)
		metadata, metadataErr := readCertificateMetadata(cfg.Server.HTTPS.CertFile, cfg.Server.HTTPS.KeyFile)
		writeJSON(w, http.StatusOK, map[string]any{"https_enabled": cfg.Server.HTTPS.Enabled, "certificate_exists": certErr == nil, "key_exists": keyErr == nil, "metadata_exists": metadataErr == nil, "metadata": metadata, "cert_file": cfg.Server.HTTPS.CertFile, "key_file": cfg.Server.HTTPS.KeyFile})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req certificateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.IPAddresses = splitValues(req.IPAddresses)
	for _, value := range req.IPAddresses {
		if net.ParseIP(value) == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid IP address " + value + "; use individual IPs, not CIDR or ranges"})
			return
		}
	}
	if req.ValidityDays < 1 || req.ValidityDays > maxCertificateValidityDays {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("validity_days must be between 1 and %d", maxCertificateValidityDays)})
		return
	}
	if req.CommonName == "" && len(req.DNSNames) == 0 && len(req.IPAddresses) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one common name, DNS name or IP address is required"})
		return
	}
	if err := generateCertificate(cfg.Server.HTTPS.CertFile, cfg.Server.HTTPS.KeyFile, req); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "cert_file": cfg.Server.HTTPS.CertFile, "key_file": cfg.Server.HTTPS.KeyFile, "metadata_file": certificateMetadataPath(cfg.Server.HTTPS.CertFile), "https_enabled": cfg.Server.HTTPS.Enabled})
}

func splitValues(values []string) []string {
	out := []string{}
	for _, value := range values {
		out = append(out, strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })...)
	}
	return out
}

func generateCertificate(certPath, keyPath string, req certificateRequest) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	subject := req.CommonName
	if subject == "" && len(req.DNSNames) > 0 {
		subject = req.DNSNames[0]
	}
	if subject == "" {
		subject = req.IPAddresses[0]
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: subject}, DNSNames: req.DNSNames, IPAddresses: parseIPs(req.IPAddresses), NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(time.Duration(req.ValidityDays) * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return err
	}
	metadata := certificateMetadata{CommonName: req.CommonName, DNSNames: req.DNSNames, IPAddresses: req.IPAddresses, ValidityDays: req.ValidityDays, GeneratedAt: now, CertFile: certPath, KeyFile: keyPath}
	if metadata.CommonName == "" {
		metadata.CommonName = subject
	}
	return writeCertificateMetadata(certificateMetadataPath(certPath), metadata)
}

func certificateMetadataPath(certPath string) string {
	return filepath.Join(filepath.Dir(certPath), "pve-web-certificate.json")
}

func writeCertificateMetadata(metadataPath string, metadata certificateMetadata) error {
	b, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	tmp := metadataPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, metadataPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readCertificateMetadata(certPath, keyPath string) (*certificateMetadata, error) {
	if _, err := os.Stat(certPath); err != nil {
		return nil, err
	}
	if _, err := os.Stat(keyPath); err != nil {
		return nil, err
	}
	metadataPath := certificateMetadataPath(certPath)
	if b, err := os.ReadFile(metadataPath); err == nil {
		var metadata certificateMetadata
		if err := json.Unmarshal(b, &metadata); err == nil {
			return &metadata, nil
		}
	}
	return readCertificateMetadataFromCert(certPath)
}

func readCertificateMetadataFromCert(certPath string) (*certificateMetadata, error) {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	ipAddresses := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		ipAddresses = append(ipAddresses, ip.String())
	}
	validityDays := int(cert.NotAfter.Sub(cert.NotBefore) / (24 * time.Hour))
	if cert.NotAfter.Sub(cert.NotBefore)%(24*time.Hour) != 0 {
		validityDays++
	}
	return &certificateMetadata{CommonName: cert.Subject.CommonName, DNSNames: cert.DNSNames, IPAddresses: ipAddresses, ValidityDays: validityDays, GeneratedAt: cert.NotBefore, CertFile: certPath, KeyFile: ""}, nil
}

func parseIPs(values []string) []net.IP {
	out := make([]net.IP, 0, len(values))
	for _, value := range values {
		if ip := net.ParseIP(value); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
func writeAPIError(w http.ResponseWriter, err error) {
	var apiErr *proxmox.APIError
	if errors.As(err, &apiErr) {
		kind := "proxmox_api"
		if apiErr.Kind == "connection" {
			kind = "connection"
		}
		if apiErr.Status == 401 {
			kind = "authentication"
		}
		if apiErr.Status == 403 {
			kind = "permission"
		}
		writeJSON(w, 502, map[string]any{"error": map[string]string{"kind": kind, "message": apiErr.Error()}})
		return
	}
	writeJSON(w, 500, map[string]map[string]string{"error": {"kind": "internal", "message": err.Error()}})
}
func RefreshContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}
