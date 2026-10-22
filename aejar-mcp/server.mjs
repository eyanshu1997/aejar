#!/usr/bin/env node
// aejar MCP server, run by Claude Code inside each session container.
//
// It is two things:
//  - a channel: aejard writes JSON lines to a Unix socket and they arrive in
//    the running session as <channel source="aejar"> events (owner messages,
//    review comments, approval results);
//  - tools: request_approval and get_approval, which call aejard with this
//    session's token. Approval is decided by the owner, never here.
import { Server } from '@modelcontextprotocol/sdk/server/index.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js'
import net from 'node:net'
import fs from 'node:fs'

const AEJAR_URL = process.env.AEJAR_URL ?? 'http://aejard:9111'
const TOKEN = process.env.AEJAR_SESSION_TOKEN ?? ''
const SOCKET = process.env.AEJAR_CHANNEL_SOCKET ?? '/run/aejar/channel.sock'

// Channel meta keys must be identifiers; values must be strings.
const META_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/

const log = (...a) => process.stderr.write(`aejar-mcp: ${a.join(' ')}\n`)

async function aejar(method, path, body) {
  const res = await fetch(AEJAR_URL + path, {
    method,
    headers: { authorization: `Bearer ${TOKEN}`, 'content-type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  let data
  try { data = text ? JSON.parse(text) : {} } catch { data = { message: text } }
  if (!res.ok) throw new Error(data.message ?? `aejard ${res.status}`)
  return data
}

const mcp = new Server(
  { name: 'aejar', version: '0.1.0' },
  {
    capabilities: { experimental: { 'claude/channel': {} }, tools: {} },
    instructions:
      'Events from aejar arrive as <channel source="aejar" kind="..." event_id="...">. ' +
      'kind=chat or comment: a message or review comment from the owner; act on it (for comments on a doc, ' +
      'reload the doc and revise it). kind=action_result: the outcome of an approval you requested. ' +
      'Use request_approval before any action that changes something outside this container that other ' +
      'people can see or that cannot be undone, then end your turn and wait for the action_result event.',
  },
)

const tools = [
  {
    name: 'request_approval',
    description:
      "Ask the owner to approve an action. This does NOT perform the action: it queues a request and returns " +
      "an id. For a blocked tool call, pass the exact tool_name and tool_input; once approved, retry that exact " +
      "call once. For a yes/no decision with no tool, use class question. After calling this, end your turn.",
    inputSchema: {
      type: 'object',
      properties: {
        summary: { type: 'string', description: 'One or two sentences the owner reads on the approval card' },
        class: { type: 'string', enum: ['action', 'question'], description: 'action (a specific tool call) or question' },
        tool_name: { type: 'string', description: 'For class action: the exact tool name that was blocked' },
        tool_input: { type: 'object', description: 'For class action: the exact tool input, unchanged' },
      },
      required: ['summary'],
    },
  },
  {
    name: 'complete_task',
    description:
      'Mark this task done when its work is finished (not while waiting for an approval or an answer). ' +
      'The session ends after this turn; the owner can reopen it by sending a message.',
    inputSchema: {
      type: 'object',
      properties: { summary: { type: 'string', description: 'What was done, in one to three sentences' } },
      required: ['summary'],
    },
  },
  {
    name: 'get_approval',
    description: 'Check the state of an approval request by id.',
    inputSchema: { type: 'object', properties: { id: { type: 'string' } }, required: ['id'] },
  },
]

mcp.setRequestHandler(ListToolsRequestSchema, async () => ({ tools }))

mcp.setRequestHandler(CallToolRequestSchema, async (req) => {
  const args = req.params.arguments ?? {}
  try {
    if (req.params.name === 'request_approval') {
      const action = (args.class ?? (args.tool_name ? 'action' : 'question')) === 'action'
      const a = await aejar('POST', '/v1/approvals', {
        class: action ? 'APPROVAL_CLASS_ACTION' : 'APPROVAL_CLASS_QUESTION',
        summary: args.summary,
        toolName: action ? args.tool_name : undefined,
        payload: action ? (args.tool_input ?? {}) : {},
      })
      return text(
        `Approval ${a.id} is pending with the owner. Do not retry or work around the blocked action. ` +
        'End your turn now; the result arrives as a channel event.',
      )
    }
    if (req.params.name === 'complete_task') {
      await aejar('POST', '/hooks/claude/task-complete', { message: args.summary ?? '' })
      return text('Task marked complete. Finish your reply; the session ends after this turn.')
    }
    if (req.params.name === 'get_approval') {
      const a = await aejar('GET', `/v1/approvals/${encodeURIComponent(args.id)}`)
      return text(`Approval ${a.id}: ${a.state}${a.reason ? ` (${a.reason})` : ''}`)
    }
    throw new Error(`unknown tool ${req.params.name}`)
  } catch (err) {
    return { isError: true, content: [{ type: 'text', text: String(err.message ?? err) }] }
  }
})

function text(t) {
  return { content: [{ type: 'text', text: t }] }
}

async function emit(line) {
  let ev
  try { ev = JSON.parse(line) } catch { log('dropping non-JSON line'); return }
  if (typeof ev.content !== 'string') return
  const meta = {}
  for (const [k, v] of Object.entries({ event_id: ev.id, kind: ev.kind, ...(ev.meta ?? {}) })) {
    if (META_KEY.test(k) && v !== undefined && v !== null) meta[k] = String(v)
  }
  await mcp.notification({ method: 'notifications/claude/channel', params: { content: ev.content, meta } })
}

await mcp.connect(new StdioServerTransport())

// Only aejard can reach this socket (it lives in the session's run dir).
try { fs.unlinkSync(SOCKET) } catch {}
const server = net.createServer((conn) => {
  let buf = ''
  conn.setEncoding('utf8')
  conn.on('data', async (chunk) => {
    buf += chunk
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim()
      buf = buf.slice(i + 1)
      if (line) await emit(line)
    }
  })
  conn.on('end', () => { if (buf.trim()) emit(buf.trim()) })
  conn.on('error', () => {})
})
// Readiness is reported by the session entrypoint, not here: this process
// cannot tell whether Claude Code actually registered it as a channel.
server.listen(SOCKET, () => log(`channel listening on ${SOCKET}`))
