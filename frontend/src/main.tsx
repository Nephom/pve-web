import React, { Suspense, useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import './style.css'

const NodeChart = React.lazy(() => import('./charts').then(m => ({ default: m.NodeChart })))
const GuestChart = React.lazy(() => import('./charts').then(m => ({ default: m.GuestChart })))

type Guest = { vmid: number; type: string; node: string; name: string; status: string; cpu: number; mem: number; maxmem: number }
type Node = { node: string; status: string; cpu: number; mem: number; maxmem: number }
type Sample = { at: string; cpu: number; memory: number }
type Storage = { storage: string; type: string; active: number; status: string; total: number | null; used: number | null; avail: number | null }
type State = { id: string; name: string; type: string; nodes: Node[]; guests: Guest[]; storages: Record<string, Storage[]>; samples: Record<string, Sample[]>; ceph?: CephSummary; error?: string; error_kind?: string; last_refresh?: string }
type CephSummary = { health: string; details: string[]; total: number; up: number; in: number; problems: { id: number; hostname?: string; status?: string; in: boolean; up: boolean }[] }
type SelectedGuest = { guest: Guest; action?: string } | null
type TargetConfig = { id: string; name: string; type: string; enabled: boolean; verify_tls: boolean; detect_ha?: boolean; detect_ceph?: boolean; endpoints: string[]; user: string; token_name: string; token_value?: string; credential_configured?: boolean; console_user?: string; console_password?: string; console_configured?: boolean }
type ConsoleSession = { node: string; websocket_path: string; user: string; ticket: string } | null
type GuestConsoleSession = { node: string; type: string; vmid: number; websocket_path: string; password?: string } | null
type CertificateMetadata = { common_name: string; dns_names: string[]; ip_addresses: string[]; validity_days: number; generated_at: string; cert_file: string; key_file: string }
type CertificateStatus = { certificate_exists: boolean; key_exists: boolean; cert_file: string; key_file: string; metadata?: CertificateMetadata | null }
type CertificateForm = { common_name: string; dns_names: string; ip_addresses: string; validity_days: string }

const base = '/pve-web'
const chartColors = { cpu: '#55d6ff', memory: '#9a7bff' }

function percent(value: number, max: number) { return max > 0 ? Math.round(value * 100 / max) : 0 }
function ratioPercent(value: number) { return Number((value * 100).toFixed(2)) }
function displayPercent(value: number) { return (value * 100).toFixed(2) }
function bytes(value: number | null) { if (value === null || value === undefined) return '—'; const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']; let n = value; let i = 0; while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ } return `${n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}` }
function storagePercent(s: Storage) { return s.total && s.used !== null ? Math.round(s.used * 100 / s.total) : 0 }
function statusClass(status: string) { return status === 'running' || status === 'started' || status === 'online' ? 'status-online' : status === 'stopped' ? 'status-stopped' : 'status-alert' }
function actionAllowed(status: string, action: string) { return status === 'stopped' ? action === 'start' : status === 'running' || status === 'started' ? action !== 'start' : false }

function App() {
  const [targets, setTargets] = useState<State[]>([])
  const [selected, setSelected] = useState('')
  const [notice, setNotice] = useState('')
  const [job, setJob] = useState<any>(null)
  const [certOpen, setCertOpen] = useState(false)
  const [certificateStatus, setCertificateStatus] = useState<CertificateStatus | null>(null)
  const [certificateGenerated, setCertificateGenerated] = useState(false)
  const [certificateLoaded, setCertificateLoaded] = useState(false)
  const [certificateLoadPreview, setCertificateLoadPreview] = useState<CertificateMetadata | null>(null)
  const [certificateForm, setCertificateForm] = useState<CertificateForm>({ common_name: '', dns_names: '', ip_addresses: '', validity_days: '365' })
  const [guestModal, setGuestModal] = useState<SelectedGuest>(null)
  const [powerSubmitting, setPowerSubmitting] = useState(false)
  const [targetEditor, setTargetEditor] = useState<TargetConfig | null>(null)
  const [targetList, setTargetList] = useState<TargetConfig[]>([])
  const [consoleSession, setConsoleSession] = useState<ConsoleSession>(null)
  const [guestConsole, setGuestConsole] = useState<GuestConsoleSession>(null)
  const [cephOpen, setCephOpen] = useState(false)
  const consoleRef = useRef<HTMLDivElement>(null)
  const guestConsoleRef = useRef<HTMLDivElement>(null)

  const load = () => fetch(base + '/data/overview').then(r => r.json()).then(v => {
    if (!Array.isArray(v.targets)) throw new Error(v.error || 'Invalid overview response')
    const next = v.targets.map((t: State) => ({ ...t, nodes: Array.isArray(t.nodes) ? t.nodes : [], guests: Array.isArray(t.guests) ? t.guests : [], storages: t.storages || {}, samples: t.samples || {} }))
    setTargets(next); setSelected(x => x || next[0]?.id || '')
  }).catch(e => setNotice(e.message))

  useEffect(() => { load(); const id = setInterval(load, 5000); return () => clearInterval(id) }, [])

  const loadTargetConfig = () => fetch(base + '/config/targets').then(r => r.json()).then(v => setTargetList(v.targets || []))
  useEffect(() => { loadTargetConfig() }, [])
  useEffect(() => {
    if (!consoleSession || !consoleRef.current) return
    let disposed = false
    let dispose: (() => void) | null = null
    Promise.all([import('@xterm/xterm'), import('@xterm/addon-fit'), import('@xterm/xterm/css/xterm.css')]).then(([{ Terminal }, { FitAddon }]) => {
      if (disposed || !consoleRef.current || !consoleSession) return
      const container = consoleRef.current
      container.innerHTML = ''
      const term = new Terminal({ cursorBlink: true, fontSize: 13, theme: { background: '#050a12', foreground: '#d6e6f5' } })
      const fit = new FitAddon()
      term.loadAddon(fit)
      term.open(container)
      fit.fit()
      const url = new URL(consoleSession.websocket_path, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(url.toString())
      ws.binaryType = 'arraybuffer'
      let connected = false
      let ping: number | undefined
      let unmounted = false
      const utf8Length = (value: string) => unescape(encodeURIComponent(value)).length
      const closeAll = (clean: boolean) => {
        if (unmounted) return
        setConsoleSession(null)
        setNotice(clean ? 'Node Shell disconnected' : 'Node Shell WebSocket disconnected; check the reverse proxy WebSocket settings')
      }
      ws.onopen = () => { ws.send(`${consoleSession.user}:${consoleSession.ticket}\n`) }
      ws.onmessage = event => {
        const data = new Uint8Array(event.data as ArrayBuffer)
        if (!connected) {
          if (data[0] === 79 && data[1] === 75) { // "OK"
            connected = true
            term.write(data.slice(2))
            requestAnimationFrame(() => requestAnimationFrame(() => { fit.fit(); term.focus() }))
            ping = window.setInterval(() => { if (ws.readyState === WebSocket.OPEN) ws.send('2') }, 30000)
          } else {
            ws.close()
          }
          return
        }
        term.write(data)
      }
      ws.onclose = event => closeAll(event.code === 1000)
      ws.onerror = () => closeAll(false)
      const dataListener = term.onData(value => { if (connected && ws.readyState === WebSocket.OPEN) ws.send(`0:${utf8Length(value)}:${value}`) })
      const resizeListener = term.onResize(size => { if (connected && ws.readyState === WebSocket.OPEN) ws.send(`1:${size.cols}:${size.rows}:`) })
      const onWindowResize = () => fit.fit()
      window.addEventListener('resize', onWindowResize)
      dispose = () => {
        unmounted = true
        window.removeEventListener('resize', onWindowResize)
        if (ping) window.clearInterval(ping)
        dataListener.dispose()
        resizeListener.dispose()
        ws.close()
        term.dispose()
      }
    }).catch(() => { if (!disposed) setNotice('Failed to load the terminal viewer') })
    return () => { disposed = true; if (dispose) dispose() }
  }, [consoleSession])


  useEffect(() => {
    if (!guestConsole || !guestConsoleRef.current) return
    let disposed = false
    let rfb: any = null
    import('@novnc/novnc').then(({ default: RFB }) => {
      if (disposed || !guestConsoleRef.current) return
      const url = new URL(guestConsole.websocket_path, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      rfb = new RFB(guestConsoleRef.current, url.toString(), { credentials: { password: guestConsole.password } })
      rfb.scaleViewport = true
      // noVNC only grabs keyboard focus for its canvas on a click/touch
      // inside it (see novnc/core/rfb.js _focusCanvas). Until then, focus
      // stays on whatever was focused before the modal opened (usually the
      // "Console" button that was just clicked), and pressing Enter/Space
      // re-activates that button instead of typing into the console. Grab
      // focus as soon as the connection is established, the same way the
      // Node Shell effect already calls term.focus() after its handshake.
      rfb.addEventListener('connect', () => rfb.focus())
      rfb.addEventListener('disconnect', (event: Event) => {
        setGuestConsole(null)
        const detail = (event as CustomEvent).detail
        setNotice(detail?.clean ? 'Guest console disconnected' : 'Guest console WebSocket disconnected; check the reverse proxy WebSocket settings')
      })
    }).catch(() => setNotice('Failed to load the console viewer'))
    return () => { disposed = true; if (rfb) rfb.disconnect() }
  }, [guestConsole])

  const target = targets.find(x => x.id === selected)
  const selectedAction = guestModal?.action
  const selectedGuest = guestModal?.guest

  const submitPower = (guest: Guest, action: string) => {
    if (!target || powerSubmitting) return
    setPowerSubmitting(true)
    setGuestModal({ guest, action })
    fetch(`${base}/operation/guests/${target.id}/${guest.type}/${guest.vmid}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action }) }).then(async r => { const v = await r.json(); if (!r.ok) throw new Error(typeof v.error === 'string' ? v.error : v.error?.message || `Power operation failed (${r.status})`); return v }).then(v => {
      setJob(v)
      if (v.id) { const poll = setInterval(() => fetch(base + '/tasks/' + v.id).then(x => x.json()).then(j => { setJob(j); if (j.status === 'succeeded' || j.status === 'failed') { clearInterval(poll); setPowerSubmitting(false); setGuestModal(null); load() } }), 2000) } else { setPowerSubmitting(false); setGuestModal(null) }
    }).catch(e => { setPowerSubmitting(false); setNotice(e.message); setGuestModal(null) })
  }

  const generateCertificate = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    fetch(base + '/certificates', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ common_name: certificateForm.common_name.trim(), dns_names: certificateForm.dns_names.split(',').map(x => x.trim()).filter(Boolean), ip_addresses: certificateForm.ip_addresses.split(/[,,\n\r]/).map(x => x.trim()).filter(Boolean), validity_days: Number(certificateForm.validity_days || 365) }) }).then(async r => { const v = await r.json(); if (!r.ok) throw new Error(v.error || `Certificate generation failed (${r.status})`); return v }).then(v => { setNotice(v.error || 'Certificate generated/regenerated. Download the files below.'); setCertificateStatus({ certificate_exists: true, key_exists: true, cert_file: v.cert_file, key_file: v.key_file }); setCertificateGenerated(true) }).catch(e => setNotice(e.message))
  }

  const loadPreviousCertificate = () => { fetch(base + '/certificates').then(async r => { const v = await r.json(); if (!r.ok) throw new Error(v.error || `Unable to load certificate (${r.status})`); if (!v.certificate_exists || !v.key_exists || !v.metadata) throw new Error('No previous Certificate data was found. Please generate a new certificate.'); return v.metadata as CertificateMetadata }).then(metadata => setCertificateLoadPreview(metadata)).catch(e => setNotice(e.message)) }
  const confirmCertificateLoad = () => { if (!certificateLoadPreview) return; setCertificateForm({ common_name: certificateLoadPreview.common_name || '', dns_names: (certificateLoadPreview.dns_names || []).join(', '), ip_addresses: (certificateLoadPreview.ip_addresses || []).join(', '), validity_days: String(certificateLoadPreview.validity_days || 365) }); setCertificateLoaded(true); setCertificateLoadPreview(null); setNotice('Previous certificate data loaded. Review it, then press Regenerate.') }
  const openCertificates = () => { setCertOpen(true); setCertificateGenerated(false); setCertificateLoaded(false); setCertificateStatus(null); setCertificateLoadPreview(null); setCertificateForm({ common_name: '', dns_names: '', ip_addresses: '', validity_days: '365' }) }

  const saveTarget = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault(); const form = new FormData(event.currentTarget); const id = String(form.get('id') || ''); const existing = targetEditor?.id
    const payload = { id, name: String(form.get('name') || ''), type: String(form.get('type') || 'cluster'), enabled: form.get('enabled') === 'on', verify_tls: form.get('verify_tls') === 'on', detect_ha: form.get('detect_ha') === 'on', detect_ceph: form.get('detect_ceph') === 'on', endpoints: String(form.get('endpoints') || '').split(/[,\n]/).map(x => x.trim()).filter(Boolean), user: String(form.get('user') || ''), token_name: String(form.get('token_name') || ''), token_value: String(form.get('token_value') || ''), console_user: String(form.get('console_user') || ''), console_password: String(form.get('console_password') || '') }
    fetch(existing ? `${base}/config/targets/${encodeURIComponent(existing)}` : base + '/config/targets', { method: existing ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) }).then(r => r.json()).then(v => { if (v.error) throw new Error(v.error); setTargetEditor(null); loadTargetConfig(); load() }).catch(e => setNotice(e.message))
  }

  const deleteTarget = (id: string) => { if (!window.confirm(`Delete target ${id}?`)) return; fetch(`${base}/config/targets/${encodeURIComponent(id)}`, { method: 'DELETE' }).then(r => r.json()).then(v => { if (v.error) throw new Error(v.error); loadTargetConfig(); load() }).catch(e => setNotice(e.message)) }
  const openConsole = (node: string) => { if (!target) return; fetch(`${base}/console/nodes/${encodeURIComponent(target.id)}/${encodeURIComponent(node)}`, { method: 'POST' }).then(async r => { const v = await r.json(); if (!r.ok) throw new Error(v.error?.message || v.error || `Console failed (${r.status})`); setConsoleSession({ node: v.node, websocket_path: v.websocket_path, user: v.user, ticket: v.ticket }) }).catch(e => setNotice(e.message)) }
  const openGuestConsole = (guest: Guest) => { if (!target) return; fetch(`${base}/console/guests/${encodeURIComponent(target.id)}/${encodeURIComponent(guest.node)}/${encodeURIComponent(guest.type)}/${guest.vmid}`, { method: 'POST' }).then(async r => { const v = await r.json(); if (!r.ok) throw new Error(v.error?.message || v.error || `Console failed (${r.status})`); setGuestConsole({ node: v.node, type: v.type, vmid: v.vmid, websocket_path: v.websocket_path, password: v.password }) }).catch(e => setNotice(e.message)) }

  const renderChart = (samples: Sample[]) => {
    let chart = samples.map(s => ({ ...s, time: new Date(s.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }), cpu: ratioPercent(s.cpu) }))
    return chart
  }

  const renderGuestChart = (samples: Sample[]) => {
    let chart = samples.map(s => ({ ...s, time: new Date(s.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }), cpu: ratioPercent(s.cpu) }))
    return chart
  }

  const sharedStorages = target ? Array.from(new Map(Object.values(target.storages).flat().map(s => [s.storage, s])).values()).sort((a, b) => a.storage.localeCompare(b.storage)) : []

  return <div className="shell">
    <header className="topbar"><div><span className="eyebrow">PVE / LIVE OPERATIONS</span><h1>PVE Monitor</h1><p className="subtitle">Proxmox infrastructure telemetry and guest control</p></div><div className="header-actions"><span className="live-pill"><i /> LIVE / 5s</span><button onClick={() => setTargetEditor({ id: '', name: '', type: 'cluster', enabled: true, verify_tls: false, detect_ha: true, detect_ceph: true, endpoints: [], user: '', token_name: '', console_user: 'root@pam' })}>Targets</button><button onClick={load}>Refresh</button><button onClick={openCertificates}>Certificates</button></div></header>
    {notice && <div className="notice"><span>{notice}</span><button type="button" className="notice-close" aria-label="Dismiss notification" onClick={() => setNotice('')}>×</button></div>}
    {consoleSession && <div className="overlay"><div className="modal console-modal"><button className="modal-close" onClick={() => setConsoleSession(null)}>×</button><span className="eyebrow">NODE SHELL / PVE API</span><h2>{consoleSession.node}</h2><div className="console-screen" ref={consoleRef} /></div></div>}
    {guestConsole && <div className="overlay"><div className="modal console-modal"><button className="modal-close" onClick={() => setGuestConsole(null)}>×</button><span className="eyebrow">GUEST CONSOLE / {guestConsole.type.toUpperCase()}</span><h2>{guestConsole.type.toUpperCase()} {guestConsole.vmid} on {guestConsole.node}</h2><div className="console-screen" ref={guestConsoleRef} /></div></div>}
    {cephOpen && target?.ceph && <div className="overlay"><div className="modal ceph-modal"><button className="modal-close" onClick={() => setCephOpen(false)}>×</button><span className="eyebrow">CEPH CLUSTER / HEALTH</span><h2>Ceph Health Status</h2><div className="ceph-health"><i className={`status-dot ${target.ceph.health === 'HEALTH_OK' ? 'status-online' : 'status-alert'}`} />{target.ceph.health || 'UNKNOWN'}</div><h3>Condition</h3><div className="ceph-details">{target.ceph.details.length ? target.ceph.details.map(detail => <div key={detail}>{detail}</div>) : <div>All health checks passed</div>}</div><h3>OSD Status</h3><div className="ceph-osd-summary">{target.ceph.problems.length === 0 ? 'All OK' : `${target.ceph.problems.length} problem(s)`}<span>{target.ceph.up} up / {target.ceph.in} in / {target.ceph.total} total</span></div>{target.ceph.problems.length > 0 && <div className="ceph-problems">{target.ceph.problems.map(problem => <div key={problem.id}>osd.{problem.id} {problem.status || (problem.up ? 'up' : 'down')} / {problem.in ? 'in' : 'out'} {problem.hostname || ''}</div>)}</div>}</div></div>}
    {targetEditor && <div className="overlay"><div className="modal target-modal"><button className="modal-close" onClick={() => setTargetEditor(null)}>×</button><span className="eyebrow">TARGET MANAGEMENT</span><h2>{targetEditor.id ? 'Edit target' : 'Add target'}</h2><div className="target-editor-list">{targetList.map(t => <div className="target-editor-row" key={t.id}><span><b>{t.name}</b><small>{t.id} · {t.endpoints.join(', ')}</small></span><button type="button" onClick={() => setTargetEditor({ ...t, endpoints: t.endpoints || [], token_value: '', console_password: '' })}>Edit</button><button type="button" onClick={() => deleteTarget(t.id)}>Delete</button></div>)}</div><form onSubmit={saveTarget}><div className="form-grid"><label>Target ID<input name="id" defaultValue={targetEditor.id} readOnly={Boolean(targetEditor.id)} required /></label><label>Name<input name="name" defaultValue={targetEditor.name} required /></label><label>Type<select name="type" defaultValue={targetEditor.type}><option value="cluster">Cluster</option><option value="node">Node</option></select></label><label>Proxmox endpoints<textarea name="endpoints" defaultValue={targetEditor.endpoints.join('\n')} placeholder="https://pve01.example.local:8006\nhttps://pve02.example.local:8006" required /></label><label>User<input name="user" defaultValue={targetEditor.user} placeholder="root@pam" required /></label><label>Token name<input name="token_name" defaultValue={targetEditor.token_name} placeholder="pve-web" required /></label><label className="wide">Token value<input name="token_value" type="password" placeholder={targetEditor.credential_configured ? 'Leave blank to keep current token' : 'Paste API token value'} /></label><label>Console user<input name="console_user" defaultValue={targetEditor.console_user || 'root@pam'} placeholder="root@pam" required /></label><label>Console password<input name="console_password" type="password" placeholder={targetEditor.console_configured ? 'Leave blank to keep current password' : 'PVE root@pam password'} /></label></div><div className="check-grid"><label><input type="checkbox" name="enabled" defaultChecked={targetEditor.enabled} /> Enabled</label><label><input type="checkbox" name="verify_tls" defaultChecked={targetEditor.verify_tls} /> Verify TLS</label><label><input type="checkbox" name="detect_ha" defaultChecked={targetEditor.detect_ha} /> Detect HA</label><label><input type="checkbox" name="detect_ceph" defaultChecked={targetEditor.detect_ceph} /> Detect Ceph</label></div><div className="modal-actions"><button type="submit" className="primary">Save target</button><button type="button" onClick={() => setTargetEditor(null)}>Cancel</button></div></form></div></div>}
    {certOpen && <div className="overlay"><div className="modal cert-modal"><form onSubmit={generateCertificate}><div className="modal-title"><span className="eyebrow">SECURITY</span><h2>HTTPS certificate</h2></div><p>HTTP remains available. Generate a new certificate, or load the previous local certificate data before regenerating it.</p><div className="certificate-load-actions"><button type="button" onClick={loadPreviousCertificate}>Load previous certificate</button><small>Reads the local certificate, key and saved metadata.</small></div>{certificateLoadPreview && <div className="certificate-load-confirm"><strong>Previous certificate found</strong><div className="certificate-summary"><span>Common Name<b>{certificateLoadPreview.common_name || '—'}</b></span><span>DNS names<b>{certificateLoadPreview.dns_names?.join(', ') || '—'}</b></span><span>IP addresses<b>{certificateLoadPreview.ip_addresses?.join(', ') || '—'}</b></span><span>Validity days<b>{certificateLoadPreview.validity_days}</b></span></div><div className="modal-actions"><button type="button" className="primary" onClick={confirmCertificateLoad}>OK, load data</button><button type="button" onClick={() => setCertificateLoadPreview(null)}>Cancel</button></div></div>}<label>Common Name<input name="common_name" value={certificateForm.common_name} onChange={e => setCertificateForm(f => ({ ...f, common_name: e.target.value }))} placeholder="pve-monitor.example.local" /></label><label>DNS names<input name="dns_names" value={certificateForm.dns_names} onChange={e => setCertificateForm(f => ({ ...f, dns_names: e.target.value }))} placeholder="pve-monitor.example.local, pve-monitor" /></label><label>IP addresses<textarea name="ip_addresses" value={certificateForm.ip_addresses} onChange={e => setCertificateForm(f => ({ ...f, ip_addresses: e.target.value }))} placeholder={'172.20.111.106, 172.20.111.107\n192.168.1.10'} /><small className="field-hint">Multiple IPs: separate with commas or new lines. CIDR/ranges are not valid certificate IP SANs; list each address individually.</small></label><label>Validity days<input name="validity_days" type="number" min="1" max="3650" value={certificateForm.validity_days} onChange={e => setCertificateForm(f => ({ ...f, validity_days: e.target.value }))} /><small className="field-hint">Up to 3650 days (10 years) for internal or lab use. Generating again replaces the existing certificate and key.</small></label>{certificateGenerated && certificateStatus && <div className="certificate-downloads"><strong>Download generated files</strong><small>Use these files in your Nginx SSL configuration.</small><div><a className="download-button" href={`${base}/certificates?download=cert`} download="pve-web.crt">Download certificate (.crt)</a><a className="download-button" href={`${base}/certificates?download=key`} download="pve-web.key">Download private key (.key)</a></div></div>}<div className="modal-actions"><button type="submit" className="primary">{certificateLoaded ? 'Regenerate' : 'Generate'}</button><button type="button" onClick={() => setCertOpen(false)}>Close</button></div></form></div></div>}
    {selectedGuest && target && <div className="overlay"><div className="modal guest-modal"><button className="modal-close" disabled={powerSubmitting} onClick={() => setGuestModal(null)}>×</button><span className="eyebrow">GUEST CONTROL / {selectedGuest.type.toUpperCase()}</span><h2>{selectedGuest.name || `Guest ${selectedGuest.vmid}`}</h2><div className="guest-detail-grid"><span>VMID<strong>{selectedGuest.vmid}</strong></span><span>Node<strong>{selectedGuest.node}</strong></span><span>Status<strong><i className={`status-dot ${statusClass(selectedGuest.status)}`} />{selectedGuest.status}</strong></span></div>{selectedAction ? <div className="confirm-step"><p>{powerSubmitting ? 'Waiting for the Proxmox operation to finish...' : <>Confirm <strong>{selectedAction}</strong> for {selectedGuest.type.toUpperCase()} {selectedGuest.vmid}?</>}</p><div className="modal-actions"><button className="primary" disabled={powerSubmitting} onClick={() => submitPower(selectedGuest, selectedAction)}>{powerSubmitting ? 'Submitting...' : `Confirm ${selectedAction}`}</button><button disabled={powerSubmitting} onClick={() => setGuestModal({ guest: selectedGuest })}>Cancel</button></div></div> : <><h3>Power operations</h3><div className="power-grid">{['start', 'shutdown', 'stop', 'reboot'].map(action => <button key={action} disabled={powerSubmitting || !actionAllowed(selectedGuest.status, action)} className={actionAllowed(selectedGuest.status, action) ? 'power-enabled' : ''} onClick={() => setGuestModal({ guest: selectedGuest, action })}>{action}</button>)}</div><p className="modal-hint">Actions are submitted to Proxmox and tracked as a task.</p></>}</div></div>}
    <div className="layout"><aside className="sidebar"><div className="side-heading"><span className="eyebrow">INVENTORY</span><h2>Targets</h2></div>{targets.map(t => <button className={t.id === selected ? 'target selected' : 'target'} onClick={() => setSelected(t.id)} key={t.id}><b>{t.name}</b><span>{t.type} / {t.nodes.length} nodes / {t.guests.length} guests</span></button>)}</aside>
      <main>{target ? <><section className="hero"><div><span className="eyebrow">{target.type.toUpperCase()} / TELEMETRY</span><h2>{target.name}</h2><p>{target.last_refresh ? `Last refresh ${new Date(target.last_refresh).toLocaleTimeString()} · Rolling 5 minutes` : 'Waiting for first telemetry refresh'}</p></div><div className="stat"><strong>{target.nodes.length}</strong><span>Nodes</span></div><div className="stat"><strong>{target.guests.length}</strong><span>Guests</span></div><div className="stat"><strong>{sharedStorages.length}</strong><span>Storage pools</span></div></section>
        {target.ceph && <button className="ceph-badge" onClick={() => setCephOpen(true)}><i className={`status-dot ${target.ceph.health === 'HEALTH_OK' ? 'status-online' : 'status-alert'}`} />Ceph {target.ceph.health}</button>}{target.error && <div className="error"><b>{target.error_kind || 'error'}</b><span>{target.error}</span></div>}
        <section><div className="section-heading"><div><span className="eyebrow">SYSTEM TELEMETRY</span><h2>Nodes</h2></div><span className="section-meta">LIVE · 5 SECOND INTERVAL</span></div><div className="node-grid">{target.nodes.map(n => { const chart = renderChart(target.samples[`node/${n.node}`] || []); return <article className="node-card" key={n.node}><div className="card-head"><div><h3><i className={`status-dot ${statusClass(n.status)}`} />{n.node}</h3><span>{n.status}</span></div><div className="node-actions"><span className="node-updated">{chart.length ? chart[chart.length - 1].time : 'collecting...'}</span><button className="shell-button" onClick={() => openConsole(n.node)}>Shell</button></div></div><div className="metric-row"><span>CPU <strong>{displayPercent(n.cpu)}%</strong></span><span>Memory <strong>{percent(n.mem, n.maxmem)}%</strong></span></div><div className="chart"><Suspense fallback={<div className="chart-loading" />}><NodeChart data={chart} /></Suspense></div><div className="legend"><span><i style={{ background: chartColors.cpu }} />CPU</span><span><i style={{ background: chartColors.memory }} />Memory</span><span>5 MIN WINDOW</span></div></article> })}</div></section>
        <section className="storage-section"><div className="section-heading"><div><span className="eyebrow">CAPACITY TELEMETRY</span><h2>{target.type === 'cluster' ? 'Shared Storage Pools' : 'Storage Pools'}</h2></div><span className="section-meta">SORTED BY NAME</span></div><div className="storage-grid">{sharedStorages.map(s => { const used = storagePercent(s); return <article className="storage-card" key={s.storage}><div className="card-head"><div><h3>{s.storage}</h3><span>{s.type} · {s.status || (s.active ? 'active' : 'inactive')}</span></div><strong className={used >= 85 ? 'storage-danger' : used >= 70 ? 'storage-warning' : ''}>{used}%</strong></div><div className="capacity"><i className={used >= 85 ? 'danger' : used >= 70 ? 'warning' : ''} style={{ width: `${Math.min(used, 100)}%` }} /></div><div className="storage-numbers"><span>Used <b>{bytes(s.used)}</b></span><span>Available <b>{bytes(s.avail)}</b></span><span>Total <b>{bytes(s.total)}</b></span></div></article>})}</div></section>
        <section><div className="section-heading"><div><span className="eyebrow">GUEST INVENTORY</span><h2>Guests</h2></div><span className="section-meta">SELECT A VMID FOR POWER CONTROL</span></div><div className="guest-list">{target.guests.map(g => { const chart = renderGuestChart(target.samples[`guest/${g.type}/${g.vmid}`] || []); return <div className="guest-row" role="button" tabIndex={0} key={`${g.type}-${g.vmid}`} onClick={() => setGuestModal({ guest: g })} onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') setGuestModal({ guest: g }) }}><div className="guest-content"><span className="guest-id">{g.vmid}</span><span className="guest-type">{g.type.toUpperCase()}</span><span className="guest-name">{g.name || 'Unnamed guest'}</span><div className="guest-cpu"><div className="guest-cpu-label">CPU (%) <strong>{displayPercent(g.cpu)}%</strong></div><div className="guest-chart"><Suspense fallback={null}><GuestChart data={chart} /></Suspense></div></div><span className="guest-node">{g.node}</span><span className="guest-status"><i className={`status-dot ${statusClass(g.status)}`} />{g.status}</span><span className="chevron">{g.status !== 'stopped' && <button type="button" className="console-inline-btn" title="Open console" onClick={e => { e.stopPropagation(); openGuestConsole(g) }}>⌘</button>}›</span></div></div> })}</div></section>
        {job && <div className="task-strip"><span className="eyebrow">TASK</span><strong>{job.status || 'submitted'}</strong><span>{job.id || ''} {job.message || ''}</span></div>}
      </> : <div className="empty-state"><span className="eyebrow">NO TARGET</span><h2>No target configured</h2><p>Configure a Proxmox target to begin monitoring.</p></div>}</main></div>
  </div>
}

createRoot(document.getElementById('root')!).render(<React.StrictMode><App /></React.StrictMode>)
