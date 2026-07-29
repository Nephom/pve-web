package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"github.com/gorilla/websocket"
)

type Target struct {
	ID        string
	Endpoints []string
	VerifyTLS bool
}
type Client struct {
	target                      Target
	user, tokenName, tokenValue string
	consoleUser, consolePassword string
	http                        *http.Client
}
type APIError struct {
	Status               int
	Kind                 string
	Endpoint, Path, Body string
}

func (e *APIError) Error() string {
	if e.Status == 401 {
		return "authentication failed"
	}
	if e.Status == 403 {
		return "permission denied"
	}
	if e.Kind == "connection" {
		return "connection failed: " + e.Body
	}
	return fmt.Sprintf("Proxmox HTTP %d: %s", e.Status, e.Body)
}
func IsPermission(err error) bool {
	e, ok := err.(*APIError)
	return ok && (e.Status == 401 || e.Status == 403)
}

type Node struct {
	Node   string  `json:"node"`
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	Mem    int64   `json:"mem"`
	MaxMem int64   `json:"maxmem"`
	Uptime int64   `json:"uptime"`
	PVEVersion string `json:"pve_version,omitempty"`
}
type Guest struct {
	VMID   int64   `json:"vmid"`
	Type   string  `json:"type"`
	Node   string  `json:"node"`
	Name   string  `json:"name"`
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	Mem    int64   `json:"mem"`
	MaxMem int64   `json:"maxmem"`
	Uptime int64   `json:"uptime"`
	HAManaged bool  `json:"ha_managed"`
}
type HAResource struct {
	SID    string `json:"sid"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Node   string `json:"node"`
}
type Storage struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Active  int    `json:"active"`
	Status  string `json:"status"`
	Total   *int64 `json:"total"`
	Used    *int64 `json:"used"`
	Avail   *int64 `json:"avail"`
}
type TaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}
type Version struct { Version string `json:"version"`; Release string `json:"release"`; RepoID string `json:"repoid"` }
type FlexibleInt int
func (v *FlexibleInt) UnmarshalJSON(data []byte) error { var n int; if err := json.Unmarshal(data, &n); err == nil { *v = FlexibleInt(n); return nil }; var s string; if err := json.Unmarshal(data, &s); err != nil { return err }; parsed, err := strconv.Atoi(s); if err != nil { return err }; *v = FlexibleInt(parsed); return nil }
// NodeTerminal is a termproxy session used to drive an xterm.js Node Shell.
// Unlike the legacy vncshell/vncterm session, termproxy has no application-level
// idle timeout, so the shell stays connected until the client closes it.
type NodeTerminal struct { User string `json:"user"`; Ticket string `json:"ticket"`; Port FlexibleInt `json:"port"`; UPID string `json:"upid"`; authCookie string; csrf string }
// GuestVNC is a vncproxy session used to drive a noVNC guest (VM/CT) console.
// It connects directly to the guest's own VNC server, so it does not share the
// vncterm 10 second idle-shell timeout either.
type GuestVNC struct { User string `json:"user"`; Ticket string `json:"ticket"`; Password string `json:"password"`; Cert string `json:"cert"`; Port FlexibleInt `json:"port"`; UPID string `json:"upid"`; authCookie string; csrf string }
type CephOSDProblem struct { ID int `json:"id"`; Hostname string `json:"hostname,omitempty"`; Status string `json:"status"`; In bool `json:"in"`; Up bool `json:"up"` }
type CephSummary struct { Health string `json:"health"`; Details []string `json:"details"`; Total int `json:"total"`; Up int `json:"up"`; In int `json:"in"`; Problems []CephOSDProblem `json:"problems"` }

func (c *Client) GuestStatus(ctx context.Context, g Guest) (Guest, error) {
	data, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/%s/%d/status/current", url.PathEscape(g.Node), g.Type, g.VMID), nil)
	if err != nil {
		return Guest{}, err
	}
	var current Guest
	if err := json.Unmarshal(data, &current); err != nil {
		return Guest{}, err
	}
	current.Node, current.Type, current.VMID, current.Name = g.Node, g.Type, g.VMID, g.Name
	return current, nil
}

func NewClient(t Target, user, tokenName, tokenValue string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: !t.VerifyTLS}
	return &Client{target: t, user: user, tokenName: tokenName, tokenValue: tokenValue, http: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
} //nolint:gosec
func NewClientWithConsole(t Target, user, tokenName, tokenValue, consoleUser, consolePassword string) *Client {
	c := NewClient(t, user, tokenName, tokenValue)
	c.consoleUser, c.consolePassword = consoleUser, consolePassword
	return c
}
func (c *Client) request(ctx context.Context, method, path string, body url.Values) (json.RawMessage, error) {
	return c.requestRawWithHeaders(ctx, method, path, body, http.Header{"Authorization": []string{"PVEAPIToken=" + c.user + "!" + c.tokenName + "=" + c.tokenValue}})
}
func (c *Client) requestRaw(ctx context.Context, method, path string, body url.Values) (json.RawMessage, error) {
	return c.requestRawWithHeaders(ctx, method, path, body, nil)
}
func (c *Client) requestRawWithHeaders(ctx context.Context, method, path string, body url.Values, headers http.Header) (json.RawMessage, error) {
	var last error
	for _, endpoint := range c.target.Endpoints {
		var reader io.Reader
		if body != nil {
			reader = strings.NewReader(body.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+"/api2/json"+path, reader)
		if err != nil {
			last = err
			continue
		}
		for key, values := range headers { for _, value := range values { req.Header.Add(key, value) } }
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			last = &APIError{Kind: "connection", Endpoint: endpoint, Path: path, Body: err.Error()}
			continue
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			last = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			e := &APIError{Status: resp.StatusCode, Endpoint: endpoint, Path: path, Body: strings.TrimSpace(string(data))}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return nil, e
			}
			last = e
			continue
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			last = err
			continue
		}
		return envelope.Data, nil
	}
	return nil, fmt.Errorf("target %s: %w", c.target.ID, last)
}
func (c *Client) Version(ctx context.Context) (Version, error) {
	d, err := c.request(ctx, http.MethodGet, "/version", nil)
	var v Version
	if err == nil { err = json.Unmarshal(d, &v) }
	return v, err
}
// NodeTermProxy creates a Proxmox termproxy session for the Node Shell.
// termproxy (paired with an xterm.js frontend) has no idle-shell timeout,
// unlike the legacy vncshell/vncterm path which the PVE API hardcodes to a
// 10 second idle timeout (see PVE::API2::Nodes::vncshell, "-timeout 10").
func (c *Client) NodeTermProxy(ctx context.Context, node string) (NodeTerminal, error) {
	if c.consoleUser == "" { c.consoleUser = "root@pam" }
	if c.consolePassword == "" { return NodeTerminal{}, fmt.Errorf("console password is not configured") }
	auth, err := c.loginTicket(ctx)
	if err != nil { return NodeTerminal{}, err }
	path := fmt.Sprintf("/nodes/%s/termproxy", url.PathEscape(node))
	d, err := c.requestTicket(ctx, http.MethodPost, path, nil, auth)
	var v NodeTerminal
	if err == nil { err = json.Unmarshal(d, &v) }
	if err == nil { v.authCookie = auth.Ticket; v.csrf = auth.CSRF }
	return v, err
}
func (c *Client) DialNodeTermProxy(ctx context.Context, node string, session NodeTerminal) (*websocket.Conn, error) {
	return c.dialVNCWebsocket(ctx, node, session.Ticket, int(session.Port), session.authCookie, session.csrf)
}

// GuestVNCProxy creates a Proxmox vncproxy session for a VM (qemu) or CT (lxc)
// guest console. This connects directly to the guest's own VNC server (QEMU's
// built-in VNC server, or the LXC console proxy) and is not affected by the
// vncterm idle-shell timeout, since vncterm is only used for the Node Shell.
func (c *Client) GuestVNCProxy(ctx context.Context, node, guestType string, vmid int64, width, height int) (GuestVNC, error) {
	if c.consoleUser == "" { c.consoleUser = "root@pam" }
	if c.consolePassword == "" { return GuestVNC{}, fmt.Errorf("console password is not configured") }
	auth, err := c.loginTicket(ctx)
	if err != nil { return GuestVNC{}, err }
	path := fmt.Sprintf("/nodes/%s/%s/%d/vncproxy?websocket=1&width=%d&height=%d", url.PathEscape(node), url.PathEscape(guestType), vmid, width, height)
	d, err := c.requestTicket(ctx, http.MethodPost, path, nil, auth)
	var v GuestVNC
	if err == nil { err = json.Unmarshal(d, &v) }
	if err == nil { v.authCookie = auth.Ticket; v.csrf = auth.CSRF }
	return v, err
}
func (c *Client) DialGuestVNC(ctx context.Context, node string, session GuestVNC) (*websocket.Conn, error) {
	return c.dialVNCWebsocket(ctx, node, session.Ticket, int(session.Port), session.authCookie, session.csrf)
}

func (c *Client) dialVNCWebsocket(ctx context.Context, node, ticket string, port int, authCookie, csrf string) (*websocket.Conn, error) {
	if len(c.target.Endpoints) == 0 { return nil, fmt.Errorf("target has no endpoint") }
	endpoint := strings.TrimRight(c.target.Endpoints[0], "/")
	endpoint = strings.Replace(endpoint, "https://", "wss://", 1)
	endpoint = strings.Replace(endpoint, "http://", "ws://", 1)
	wsURL := endpoint + "/api2/json/nodes/" + url.PathEscape(node) + "/vncwebsocket?vncticket=" + url.QueryEscape(ticket) + "&port=" + fmt.Sprint(port)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: !c.target.VerifyTLS}
	dialer := websocket.Dialer{NetDialContext: tr.DialContext, TLSClientConfig: tr.TLSClientConfig}
	headers := http.Header{"Cookie": []string{"PVEAuthCookie=" + authCookie}}
	if csrf != "" { headers.Set("CSRFPreventionToken", csrf) }
	ws, _, err := dialer.DialContext(ctx, wsURL, headers)
	return ws, err
}

type authTicket struct { Ticket string `json:"ticket"`; CSRF string `json:"CSRFPreventionToken"`; Username string `json:"username"` }
func (c *Client) loginTicket(ctx context.Context) (authTicket, error) {
	data, err := c.requestRaw(ctx, http.MethodPost, "/access/ticket", url.Values{"username": []string{c.consoleUser}, "password": []string{c.consolePassword}})
	if err != nil { return authTicket{}, err }
	var auth authTicket
if err := json.Unmarshal(data, &auth); err != nil { return authTicket{}, err }
	if auth.Ticket == "" { return authTicket{}, fmt.Errorf("PVE authentication returned no ticket") }
	return auth, nil
}
func (c *Client) requestTicket(ctx context.Context, method, path string, body url.Values, auth authTicket) (json.RawMessage, error) {
	headers := http.Header{"Cookie": []string{"PVEAuthCookie=" + auth.Ticket}}
	if auth.CSRF != "" { headers.Set("CSRFPreventionToken", auth.CSRF) }
	return c.requestRawWithHeaders(ctx, method, path, body, headers)
}
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	d, e := c.request(ctx, http.MethodGet, "/nodes", nil)
	var v []Node
	if e == nil {
		e = json.Unmarshal(d, &v)
	}
	return v, e
}
func (c *Client) Guests(ctx context.Context) ([]Guest, error) {
	d, e := c.request(ctx, http.MethodGet, "/cluster/resources?type=vm", nil)
	var v []Guest
	if e == nil {
		e = json.Unmarshal(d, &v)
	}
	return v, e
}
func (c *Client) HAResources(ctx context.Context) ([]HAResource, error) {
	d, e := c.request(ctx, http.MethodGet, "/cluster/ha/resources", nil)
	var v []HAResource
	if e == nil { e = json.Unmarshal(d, &v) }
	return v, e
}
func (c *Client) CephSummary(ctx context.Context) (CephSummary, error) {
	data, err := c.request(ctx, http.MethodGet, "/cluster/ceph/status", nil)
	if err != nil { return CephSummary{}, err }
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil { return CephSummary{}, err }
	out := CephSummary{Details: []string{}, Problems: []CephOSDProblem{}}
	if health, ok := raw["health"].(map[string]any); ok {
		if value, ok := health["status"].(string); ok { out.Health = value }
		if out.Health == "" { if value, ok := health["overall_status"].(string); ok { out.Health = value } }
		if details, ok := health["detail"].([]any); ok { for _, detail := range details { if text, ok := detail.(string); ok { out.Details = append(out.Details, text) } } }
		if checks, ok := health["checks"].(map[string]any); ok { for key := range checks { out.Details = append(out.Details, key) } }
	}
	if osdmap, ok := raw["osdmap"].(map[string]any); ok { out.Total = intValue(osdmap["num_osds"]); out.Up = intValue(osdmap["num_up_osds"]); out.In = intValue(osdmap["num_in_osds"]) }
	if metadata, metadataErr := c.request(ctx, http.MethodGet, "/cluster/ceph/metadata?scope=all", nil); metadataErr == nil {
		var meta map[string]any
		if json.Unmarshal(metadata, &meta) == nil { if list, ok := meta["osd"].([]any); ok { for _, item := range list { if osd, ok := item.(map[string]any); ok { up, upOK := boolValue(osd["up"]); in, inOK := boolValue(osd["in"]); if (upOK && !up) || (inOK && !in) { out.Problems = append(out.Problems, CephOSDProblem{ID: intValue(osd["id"]), Hostname: stringValue(osd["hostname"]), Status: stringValue(osd["status"]), Up: up, In: in}) } } } } }
	}
	return out, nil
}
func intValue(v any) int { switch n := v.(type) { case float64: return int(n); case int: return n; default: return 0 } }
func boolValue(v any) (bool, bool) { b, ok := v.(bool); return b, ok }
func stringValue(v any) string { s, _ := v.(string); return s }
func (c *Client) Storages(ctx context.Context, node string) ([]Storage, error) {
	d, e := c.request(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/storage", nil)
	var v []Storage
	if e == nil {
		e = json.Unmarshal(d, &v)
	}
	return v, e
}
func (c *Client) GuestPower(ctx context.Context, g Guest, action string) (string, error) {
	switch action {
	case "start", "shutdown", "stop", "reboot":
	default:
		return "", fmt.Errorf("unsupported action")
	}
	if g.HAManaged && (action == "start" || action == "stop" || action == "shutdown") {
		state := "started"
		if action != "start" { state = "stopped" }
		sid := "vm:" + fmt.Sprint(g.VMID)
		if g.Type == "lxc" { sid = "ct:" + fmt.Sprint(g.VMID) }
		d, e := c.request(ctx, http.MethodPut, "/cluster/ha/resources/"+url.PathEscape(sid), url.Values{"state": []string{state}})
		var id string
		if e == nil && len(d) > 0 && string(d) != "null" { e = json.Unmarshal(d, &id) }
		return id, e
	}
	d, e := c.request(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%d/status/%s", url.PathEscape(g.Node), g.Type, g.VMID, action), nil)
	var id string
	if e == nil {
		e = json.Unmarshal(d, &id)
	}
	return id, e
}
func (c *Client) TaskStatus(ctx context.Context, node, upid string) (TaskStatus, error) {
	d, e := c.request(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/tasks/"+url.PathEscape(upid)+"/status", nil)
	var v TaskStatus
	if e == nil {
		e = json.Unmarshal(d, &v)
	}
	return v, e
}
