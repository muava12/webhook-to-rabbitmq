import { useState, useEffect, useCallback } from 'react'

const API = ''

// Types
interface Source { id: string; name: string; path: string; auth_token: string; routes: Route[]; created_at: number }
interface Route { id: string; source_id: string; exchange: string; routing_key: string; device_filter: string; filter_field: string; enabled: boolean }
interface Queue { name: string; messages_ready: number; messages_unacknowledged: number; consumers: number }
interface Health { rabbitmq: boolean; queued: number; rabbitmq_connected?: boolean; rmq_api_reachable?: boolean; buffered_files?: number; source_count?: number }
interface RMQCfg { host: string; port: string; user: string; password: string; vhost: string; exchange: string }
interface Config { version: number; rabbitmq: RMQCfg; sources: Source[] }

type Tab = 'sources' | 'queues' | 'tester' | 'settings'

async function api<T>(url: string, opts?: RequestInit): Promise<T> {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(r.statusText)
  return r.json()
}

function qtype(n: string) { return n.endsWith('_retry') ? 'r' : n.endsWith('_dlq') ? 'd' : 'm' }
function qlabel(n: string) { return { r: 'retry', d: 'dlq', m: 'main' }[qtype(n)] }

export default function App() {
  const [tab, setTab] = useState<Tab>('sources')
  const [health, setHealth] = useState<Health>({ rabbitmq: false, queued: 0 })
  const [config, setConfig] = useState<Config>({ version: 1, rabbitmq: {} as RMQCfg, sources: [] })
  const [queues, setQueues] = useState<Queue[]>([])
  const [toast, setToast] = useState('')
  const [rmqCfg, setRmqCfg] = useState<RMQCfg>({} as RMQCfg)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [routeForms, setRouteForms] = useState<Record<string, { filter: string; key: string; field: string }>>({})
  const [srcForm, setSrcForm] = useState({ name: '', path: '', show: false })
  const [tester, setTester] = useState({ path: '/webhook/myapp', body: '{}', showBody: false, resp: '' })

  const showToast = useCallback((m: string, err?: boolean) => {
    setToast(m); setTimeout(() => setToast(''), 3000)
  }, [])

  const load = useCallback(async () => {
    try {
      const s = await api<Health>(API + '/api/status')
      setHealth({
        rabbitmq: s.rabbitmq_connected ?? false,
        queued: s.buffered_files ?? 0,
        rabbitmq_connected: s.rabbitmq_connected,
        rmq_api_reachable: s.rmq_api_reachable,
        buffered_files: s.buffered_files,
        source_count: s.source_count
      })
    } catch { setHealth({ rabbitmq: false, queued: 0 }) }
    try { setConfig(await api<Config>(API + '/api/config')) } catch {}
    try { setQueues(await api<Queue[]>(API + '/api/queues')) } catch {}
  }, [])

  const loadRMQ = useCallback(async () => {
    try { setRmqCfg(await api<RMQCfg>(API + '/api/rmq')) } catch {}
  }, [])

  useEffect(() => { load(); loadRMQ(); const i = setInterval(load, 8000); return () => clearInterval(i) }, [load, loadRMQ])

  const toggleExpand = (id: string) => {
    setExpanded(prev => {
      const n = new Set(prev)
      if (n.has(id)) n.delete(id); else n.add(id)
      return n
    })
  }

  const qStats = {
    messages: queues.reduce((a, b) => a + (b.messages_ready || 0), 0),
    consumers: queues.reduce((a, b) => a + (b.consumers || 0), 0),
    count: queues.length,
    buffered: health.queued ?? 0
  }

  const tabLabel: Record<Tab, string> = { sources: 'Sources', queues: 'Queues', tester: 'Tester', settings: 'Settings' }

  return (
    <div className="app" style={{ position: 'relative' }}>
      <aside className="sidebar">
        <h1>Webhook · FW</h1>
        <nav className="tab-list">
          {(['sources', 'queues', 'tester'] as Tab[]).map(t => (
            <div key={t} className={'tab' + (tab === t ? ' active' : '')} onClick={() => { setTab(t); if (t === 'settings') loadRMQ() }}>
              <span>{t === 'sources' ? '📡' : t === 'queues' ? '📦' : '🧪'}</span><span>{tabLabel[t]}</span>
            </div>
          ))}
          <div className={'tab settings' + (tab === 'settings' ? ' active' : '')} onClick={() => { setTab('settings'); loadRMQ() }}>
            <span>⚙️</span><span>Settings</span>
          </div>
        </nav>
        <div className="footer">v0.2.1</div>
      </aside>

      <main className="main">
        <div className="top">
          <h2>{tabLabel[tab]}</h2>
          <div style={{display:'flex',gap:8,flexWrap:'wrap',alignItems:'center'}}>
            <div className="badge">
              <span className={'dot ' + (health.rabbitmq ? 'green' : 'red')}></span>
              <span>RMQ {health.rabbitmq ? 'Connected' : 'Disconnected'}</span>
            </div>
            {health.rmq_api_reachable !== undefined && (
              <div className="badge">
                <span className={'dot ' + (health.rmq_api_reachable ? 'green' : 'red')}></span>
                <span>API {health.rmq_api_reachable ? 'OK' : 'Error'}</span>
              </div>
            )}
          </div>
        </div>

        {tab === 'sources' && <SourcesTab
          config={config} expanded={expanded} routeForms={routeForms}
          onToggle={toggleExpand} onReload={load}
          onRouteFormChange={setRouteForms}
          onSrcFormChange={setSrcForm} srcForm={srcForm}
          showToast={showToast}
        />}

        {tab === 'queues' && <QueuesTab stats={qStats} queues={queues} />}
        {tab === 'tester' && <TesterTab tester={tester} setTester={setTester} />}
        {tab === 'settings' && <SettingsTab rmqCfg={rmqCfg} onSave={async (c) => {
          await api(API + '/api/rmq', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(c) })
          showToast('RMQ config saved')
          setTimeout(() => loadRMQ(), 1000)
        }} onRevert={async () => {
          await api(API + '/api/rmq', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: '{}' })
          await loadRMQ()
          showToast('Reverted to env defaults')
        }} showToast={showToast} />}
      </main>

      {toast && <div className={'toast' + (toast.includes('Failed') ? ' err' : '')}>{toast}</div>}
    </div>
  )
}

// ===== Sources Tab =====
function SourcesTab({ config, expanded, routeForms, onToggle, onReload, onRouteFormChange, onSrcFormChange, srcForm, showToast }: {
  config: Config; expanded: Set<string>; routeForms: Record<string, { filter: string; key: string; field: string }>
  onToggle: (id: string) => void; onReload: () => void
  onRouteFormChange: (f: Record<string, { filter: string; key: string; field: string }>) => void
  onSrcFormChange: (f: typeof srcForm) => void; srcForm: { name: string; path: string; show: boolean }
  showToast: (m: string, e?: boolean) => void
}) {
  const srcs = config.sources || []

  return (
    <>
      <div className="sec"><h3>Webhook Sources</h3>
        <button className="btn btn-sm" onClick={() => onSrcFormChange({ ...srcForm, show: !srcForm.show })}>
          + New Source
        </button>
      </div>
      <div className="help" style={{ marginBottom: 12 }}>Each source is an HTTP endpoint. Routes distribute messages based on device filter.</div>

      {srcForm.show && (
        <div className="frow">
          <div>
            <label>Name</label>
            <input className="in in-m" value={srcForm.name} onChange={e => onSrcFormChange({ ...srcForm, name: e.target.value })}
              placeholder="e.g. myapp" style={{ width: 180 }} autoFocus />
          </div>
          <div>
            <label>Webhook path</label>
            <input className="in in-m" value={srcForm.path} onChange={e => onSrcFormChange({ ...srcForm, path: e.target.value })}
              placeholder="/webhook/myapp" style={{ width: 250 }} />
          </div>
          <button className="btn btn-sm" style={{ marginTop: 12 }} onClick={async () => {
            if (!srcForm.name.trim()) { showToast('Name required', true); return }
            await api(API + '/api/sources', {
              method: 'POST', headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ name: srcForm.name.trim(), path: srcForm.path.trim() || '/webhook/' + srcForm.name.trim() })
            })
            onSrcFormChange({ name: '', path: '', show: false })
            showToast('Created'); onReload()
          }}>Create</button>
          <button className="btn btn-sm btno" style={{ marginTop: 12 }} onClick={() => onSrcFormChange({ name: '', path: '', show: false })}>Cancel</button>
        </div>
      )}

      {srcs.length === 0 && <div style={{ padding: 24, textAlign: 'center', color: 'var(--muted)' }}>No webhook sources yet.</div>}

      {srcs.map(s => (
        <div key={s.id} className="sc">
          <div className="sh" onClick={() => onToggle(s.id)}>
            <div><div className="sp">{s.path}</div><div className="sn">{s.name}</div></div>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <span className="sb">{s.routes.length} route{s.routes.length !== 1 ? 's' : ''}</span>
              <button className="btn btn-sm btno" onClick={async (e) => {
                e.stopPropagation()
                await api(API + '/api/sources/' + s.id, { method: 'DELETE' })
                showToast('Deleted'); onReload()
              }}>✕</button>
            </div>
          </div>

          {expanded.has(s.id) && (
            <div className="sbody">
              <div className="help" style={{ margin: '12px 0 8px', padding: 8, background: 'var(--bg)', borderRadius: 4, border: '1px solid var(--border)' }}>
                <strong>Device filter</strong> → <strong>routing key</strong>:
                <code> *</code> = all, <code>abc</code> = exact, <code>s23*</code> = prefix
              </div>

              {s.routes.length === 0 && <div style={{ padding: 8, color: 'var(--muted)', fontSize: 12 }}>No routes yet.</div>}

              {s.routes.map(r => (
                <div key={r.id} className="route">
                  <span className="rf">{r.device_filter}</span>
                  <span className="rk">→ {r.routing_key}</span>
                  <span className="help">{r.exchange || 'def'}</span>
                  <span className="help">{r.filter_field || 'device_id'}</span>
                  <button className="rdel" onClick={async () => {
                    await api(API + '/api/routes/' + r.id, { method: 'DELETE' })
                    showToast('Route deleted'); onReload()
                  }}>✕</button>
                </div>
              ))}

              <div className="frow" style={{ marginTop: 8 }}>
                <div><label>Device filter</label>
                  <input className="in in-m" value={routeForms[s.id]?.filter || ''}
                    onChange={e => onRouteFormChange({ ...routeForms, [s.id]: { ...routeForms[s.id], filter: e.target.value, key: routeForms[s.id]?.key || '', field: routeForms[s.id]?.field || 'device_id' } })}
                    placeholder='"*", "s23*", "abc"' style={{ width: 140 }} /></div>
                <div><label>Routing key</label>
                  <input className="in in-m" value={routeForms[s.id]?.key || ''}
                    onChange={e => onRouteFormChange({ ...routeForms, [s.id]: { ...routeForms[s.id], key: e.target.value, field: routeForms[s.id]?.field || 'device_id', filter: routeForms[s.id]?.filter || '' } })}
                    placeholder="e.g. wa.abc" style={{ width: 160 }} /></div>
                <div><label>Filter field</label>
                  <input className="in in-m" value={routeForms[s.id]?.field || 'device_id'}
                    onChange={e => onRouteFormChange({ ...routeForms, [s.id]: { ...routeForms[s.id], field: e.target.value, key: routeForms[s.id]?.key || '', filter: routeForms[s.id]?.filter || '' } })}
                    placeholder="device_id" style={{ width: 100 }} /></div>
                <button className="btn btn-sm" style={{ marginTop: 12 }} onClick={async () => {
                  const f = routeForms[s.id]
                  if (!f?.filter || !f?.key) { showToast('Filter and routing key required', true); return }
                  await api(API + '/api/routes', {
                    method: 'POST', headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ source_id: s.id, routing_key: f.key, device_filter: f.filter, filter_field: f.field || 'device_id', enabled: true })
                  })
                  onRouteFormChange({ ...routeForms, [s.id]: { filter: '', key: '', field: 'device_id' } })
                  showToast('Route added'); onReload()
                }}>+ Add Route</button>
              </div>
            </div>
          )}
        </div>
      ))}
    </>
  )
}

// ===== Queues Tab =====
function QueuesTab({ stats, queues }: { stats: { messages: number; consumers: number; count: number; buffered: number }; queues: Queue[] }) {
  return (<>
    <div className="cards">
      <div className="card"><small>Messages</small><strong className="green">{stats.messages}</strong></div>
      <div className="card"><small>Consumers</small><strong>{stats.consumers}</strong></div>
      <div className="card"><small>Queues</small><strong>{stats.count}</strong></div>
      <div className="card"><small>Buffered</small><strong className="yellow">{stats.buffered}</strong></div>
    </div>
    <table>
      <thead><tr><th>Queue</th><th>Ready</th><th>Unacked</th><th>Consumers</th></tr></thead>
      <tbody>{queues.map(q => (
        <tr key={q.name}>
          <td>{q.name}<span className={'qt ' + qtype(q.name)}>{qlabel(q.name)}</span></td>
          <td>{q.messages_ready || 0}</td>
          <td>{q.messages_unacknowledged || 0}</td>
          <td>{q.consumers || 0}</td>
        </tr>
      ))}</tbody>
    </table>
  </>)
}

// ===== Tester Tab =====
function TesterTab({ tester, setTester }: {
  tester: { path: string; body: string; showBody: boolean; resp: string }
  setTester: (t: typeof tester) => void
}) {
  return (<div className="tester">
    <div style={{ display: 'flex', gap: 8, marginBottom: 8 }}>
      <input className="in in-m" value={tester.path} onChange={e => setTester({ ...tester, path: e.target.value })}
        placeholder="/webhook/{name}" style={{ flex: 1 }} />
      <button className="btn btn-sm btno" onClick={() => setTester({ ...tester, showBody: !tester.showBody })}>
        {tester.showBody ? '−' : '+'} Body
      </button>
      <button className="btn btn-sm" onClick={async () => {
        setTester({ ...tester, resp: 'sending…' })
        try {
          const r = await fetch(tester.path, { method: 'POST', body: tester.body || '{}', headers: { 'Content-Type': 'application/json' } })
          setTester({ ...tester, resp: r.status + ' ' + r.statusText + '\n' + await r.text() })
        } catch (e: unknown) { setTester({ ...tester, resp: 'Error: ' + (e instanceof Error ? e.message : String(e)) }) }
      }}>Send</button>
    </div>
    {tester.showBody && <textarea value={tester.body} onChange={e => setTester({ ...tester, body: e.target.value })}
      placeholder='{"device_id":"abc","message":"hello"}' />}
    {tester.resp && <div className="tresp">{tester.resp}</div>}
  </div>)
}

// ===== Settings Tab =====
function SettingsTab({ rmqCfg, onSave, onRevert, showToast }: {
  rmqCfg: RMQCfg; onSave: (c: RMQCfg) => Promise<void>; onRevert: () => Promise<void>
  showToast: (m: string, e?: boolean) => void
}) {
  const [local, setLocal] = useState<RMQCfg>(rmqCfg)
  const [saving, setSaving] = useState(false)

  useEffect(() => { setLocal(rmqCfg) }, [rmqCfg])

  return (<>
    <div className="sec"><h3>RabbitMQ Connection</h3></div>
    <div className="help" style={{ marginBottom: 16 }}>
      Override env vars with custom RMQ settings. Empty = use env defaults. Save = reconnect.
    </div>
    <div className="sbox">
      {(['host', 'port', 'user', 'password', 'vhost', 'exchange'] as (keyof RMQCfg)[]).map(k => (
        <div key={k} className="f">
          <label>{k === 'password' ? 'Password' : k.charAt(0).toUpperCase() + k.slice(1)}</label>
          <input className="in in-m" type={k === 'password' ? 'password' : 'text'}
            value={local[k] || ''} onChange={e => setLocal({ ...local, [k]: e.target.value })}
            placeholder={k === 'host' ? 'e.g. 100.100.50.102' : k === 'port' ? '5672' : k === 'user' ? 'guest' : k === 'password' ? 'Enter password' : k === 'vhost' ? '/' : 'gowa'} />
          <div className="help">{
            { host: 'RabbitMQ server hostname or IP', port: 'AMQP port (default: 5672)', user: 'RabbitMQ username', password: 'RabbitMQ password', vhost: 'Virtual host (default: /)', exchange: 'Default exchange for routing' }[k]
          }</div>
        </div>
      ))}
      <div style={{ marginTop: 16, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <button className="btn" disabled={saving} onClick={async () => {
          setSaving(true); await onSave(local); setSaving(false)
        }}>{saving ? 'Saving…' : 'Save & Reconnect'}</button>
        <button className="btn btn-sm btno" onClick={async () => {
          if (!confirm('Clear RMQ config and use environment defaults?')) return
          setSaving(true); await onRevert(); setSaving(false)
        }}>↩ Use Env Defaults</button>
      </div>
    </div>
  </>)
}
