import { useEffect, useState } from 'react'
import type { MCPServer } from '../../lib/api'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { EnvVarInput } from './EnvVarInput'

export interface ServerFormData {
  name: string
  url: string
  transport?: string
  command?: string
  args?: string
  env?: string
  authType?: string
  authKey?: string
}

export function ServerFormModal({
  open,
  onClose,
  onSave,
  initial,
}: {
  open: boolean
  onClose: () => void
  onSave: (data: ServerFormData) => void
  initial?: MCPServer | null
}) {
  const [name, setName] = useState(initial?.name ?? '')
  const [url, setUrl] = useState(initial?.url ?? '')
  const [transport, setTransport] = useState(initial?.transport ?? 'sse')
  const [command, setCommand] = useState(initial?.command ?? '')
  const [args, setArgs] = useState(() => {
    if (!initial?.args) return ''
    try {
      const p = JSON.parse(initial.args)
      return Array.isArray(p) ? p.join(' ') : ''
    } catch {
      return ''
    }
  })
  const [requiredVars, setRequiredVars] = useState<Record<string, string>>({})
  const [customVars, setCustomVars] = useState<Array<{ key: string; value: string }>>([])
  const [authType, setAuthType] = useState(initial?.auth_type ?? '')
  const [authKey, setAuthKey] = useState('')
  const [authKeyTouched, setAuthKeyTouched] = useState(false)

  useEffect(() => {
    if (initial) {
      setName(initial.name)
      setUrl(initial.url)
      setTransport(initial.transport ?? 'sse')
      setCommand(initial.command ?? '')
      try {
        const p = JSON.parse(initial.args ?? '')
        setArgs(Array.isArray(p) ? p.join(' ') : '')
      } catch {
        setArgs('')
      }
      try {
        const p = JSON.parse(initial.env ?? '{}')
        setRequiredVars(typeof p === 'object' && p !== null && !Array.isArray(p) ? p : {})
      } catch {
        setRequiredVars({})
      }
      setCustomVars([])
      setAuthType(initial.auth_type ?? '')
      setAuthKey(initial.auth_key_env ?? '')
      setAuthKeyTouched(false)
    } else {
      setName('')
      setUrl('')
      setTransport('sse')
      setCommand('')
      setArgs('')
      setRequiredVars({})
      setCustomVars([])
      setAuthType('')
      setAuthKey('')
      setAuthKeyTouched(false)
    }
  }, [initial])

  if (!open) return null

  const isStdio = transport === 'stdio'

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
      <Card className="w-full max-w-lg mx-4">
        <CardHeader>
          <CardTitle>{initial ? 'Configure Server' : 'Add Server'}</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-surface-700 mb-1">Server Name</label>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g., Filesystem Server"
                className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm text-surface-900 placeholder-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-surface-700 mb-1">Transport</label>
              <select
                value={transport}
                onChange={(e) => setTransport(e.target.value)}
                className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm text-surface-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              >
                <option value="sse">SSE (Server-Sent Events)</option>
                <option value="http">HTTP (Streamable)</option>
                <option value="stdio">STDIO (Subprocess)</option>
                <option value="inline">Inline (Built-in)</option>
              </select>
            </div>
            {isStdio ? (
              <>
                <div>
                  <label className="block text-sm font-medium text-surface-700 mb-1">Command</label>
                  <input
                    type="text"
                    value={command}
                    onChange={(e) => setCommand(e.target.value)}
                    placeholder="npx"
                    className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm font-mono text-surface-900 placeholder-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-surface-700 mb-1">Args</label>
                  <input
                    type="text"
                    value={args}
                    onChange={(e) => setArgs(e.target.value)}
                    placeholder="e.g., @modelcontextprotocol/server-postgres"
                    className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm font-mono text-surface-900 placeholder-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </div>
                <EnvVarInput
                  requiredVars={requiredVars}
                  customVars={customVars}
                  onRequiredChange={(key, value) => setRequiredVars((prev) => ({ ...prev, [key]: value }))}
                  onCustomChange={setCustomVars}
                />
              </>
            ) : (
              <>
                <div>
                  <label className="block text-sm font-medium text-surface-700 mb-1">URL</label>
                  <input
                    type="text"
                    value={url}
                    onChange={(e) => setUrl(e.target.value)}
                    placeholder="https://mcp-server.example.com"
                    className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm font-mono text-surface-900 placeholder-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-surface-700 mb-1">Auth</label>
                  <select
                    value={authType}
                    onChange={(e) => setAuthType(e.target.value)}
                    className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm text-surface-900 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                  >
                    <option value="">(none)</option>
                    <option value="bearer">Bearer Token</option>
                    <option value="basic">Basic Auth</option>
                  </select>
                  {authType !== '' && (
                    <div className="mt-2">
                      <input
                        type="password"
                        value={authKey}
                        onChange={(e) => {
                          setAuthKey(e.target.value)
                          setAuthKeyTouched(true)
                        }}
                        placeholder={authType === 'basic' ? 'user:password' : 'token'}
                        className="w-full rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm font-mono text-surface-900 placeholder-surface-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                      />
                      <p className="text-xs text-surface-400 mt-1">
                        {authType === 'basic'
                          ? 'Basic auth sends the value as "Basic <base64(user:password)>". Leave blank to keep the current credential.'
                          : 'Sent as "Authorization: Bearer <token>". Leave blank to keep the current token.'}
                      </p>
                    </div>
                  )}
                </div>
              </>
            )}
            <div className="flex justify-end gap-3 pt-2">
              <Button variant="outline" onClick={onClose}>
                Cancel
              </Button>
              <Button
                onClick={() => {
                  const envObj = { ...requiredVars }
                  customVars.forEach(({ key, value }) => {
                    if (key.trim()) envObj[key.trim()] = value
                  })
                  onSave({
                    name,
                    url,
                    transport,
                    command,
                    args: JSON.stringify(args.split(' ').filter(Boolean)),
                    env: JSON.stringify(envObj),
                    authType,
                    authKey: authKeyTouched ? authKey : undefined,
                  })
                  onClose()
                }}
                disabled={!name || (isStdio ? !command : !url)}
              >
                {initial ? 'Save Changes' : 'Add Server'}
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
