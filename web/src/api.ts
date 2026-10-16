import { ApiError } from './types'
import type { M365Conversation, ModelEntry, QuotaResponse, SessionRecord } from './types'

// The gateway authenticates on a header, never on a cookie, so a cross-site
// form cannot carry the credential and no CSRF token is needed.
let apiKey = ''

export function setApiKey(key: string): void {
  apiKey = key
}

function headers(extra: Record<string, string> = {}): Record<string, string> {
  const result: Record<string, string> = { ...extra }
  if (apiKey) {
    result['Authorization'] = `Bearer ${apiKey}`
  }
  return result
}

/** Turns a failed response into an ApiError carrying the gateway's own code. */
async function toError(res: Response): Promise<ApiError> {
  let code = 'http_error'
  let message = `HTTP ${res.status}`
  try {
    const body = await res.json()
    if (body?.error) {
      code = body.error.code ?? code
      message = body.error.message ?? message
    }
  } catch {
    // A body that is not the documented error shape leaves the status as the
    // only thing worth reporting.
  }
  return new ApiError(res.status, code, message)
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: headers() })
  if (!res.ok) throw await toError(res)
  return (await res.json()) as T
}

/** What the gateway asks a person for before it will answer. */
export type AuthMode = 'none' | 'password' | 'api_key'

/**
 * Reads which gate to show.
 *
 * The page is served without a credential, so it cannot discover this by making
 * an ordinary request: with no API key configured every route answers 200, and
 * a wrong password would look like a right one.
 */
export async function fetchAuthMode(): Promise<AuthMode> {
  const body = await getJSON<{ mode?: AuthMode }>('/v1/auth')
  return body.mode ?? 'none'
}

/**
 * Asks whether a credential is one the gateway accepts.
 *
 * It travels in the same header an API client sends its key in, never in a
 * body, so it stays out of anything that records a payload. The credential is
 * an argument rather than the stored one, so a check can run before the caller
 * commits to storing it.
 */
export async function verifyCredential(credential: string): Promise<boolean> {
  const auth: Record<string, string> = credential ? { Authorization: `Bearer ${credential}` } : {}
  const res = await fetch('/v1/auth/verify', { method: 'POST', headers: auth })
  if (res.status === 401) return false
  if (!res.ok) throw await toError(res)
  return true
}

export async function listModels(): Promise<ModelEntry[]> {
  const body = await getJSON<{ data?: ModelEntry[] }>('/v1/models')
  return body.data ?? []
}

export async function listConversations(): Promise<M365Conversation[]> {
  const body = await getJSON<{ conversations?: M365Conversation[] }>('/v1/conversations')
  return body.conversations ?? []
}

export async function listSessions(): Promise<SessionRecord[]> {
  const body = await getJSON<{ data?: SessionRecord[] }>('/v1/sessions')
  return body.data ?? []
}

export interface StoredMessage {
  role: 'user' | 'assistant'
  content: string
  thinking?: string
  model?: string
  created_at: number
}

export async function loadMessages(sessionId: string): Promise<StoredMessage[]> {
  const body = await getJSON<{ data?: StoredMessage[] }>(
    `/v1/sessions/${encodeURIComponent(sessionId)}/messages`,
  )
  return body.data ?? []
}

/** Points a session id at a conversation that already exists upstream. */
export async function bindSession(sessionId: string, conversationId: string): Promise<void> {
  const res = await fetch(`/v1/sessions/${encodeURIComponent(sessionId)}`, {
    method: 'PUT',
    headers: headers({ 'Content-Type': 'application/json' }),
    body: JSON.stringify({ conversation_id: conversationId }),
  })
  if (!res.ok) throw await toError(res)
}

/**
 * Deletes the upstream conversation and clears the mapping. localOnly skips the
 * upstream delete, which a deployment without M365 web cookies needs because
 * the upstream delete can never succeed there.
 */
export async function deleteSession(sessionId: string, localOnly: boolean): Promise<void> {
  const query = localOnly ? '?local_only=true' : ''
  const res = await fetch(`/v1/sessions/${encodeURIComponent(sessionId)}${query}`, {
    method: 'DELETE',
    headers: headers(),
  })
  if (!res.ok) throw await toError(res)
}

/**
 * Reads the turns of a conversation this gateway never carried.
 *
 * The backend keeps history under the conversation id and never sends it back,
 * so a conversation started in the M365 web or mobile client opens empty here.
 * The gateway recovers it from the conversation page, which costs a download
 * and a walk of a serialization nobody here controls. That is why it runs on
 * request rather than on every open. Passing a session id also stores the
 * result, so the next open is free.
 */
export async function importHistory(
  conversationId: string,
  sessionId: string,
): Promise<StoredMessage[]> {
  const body = await getJSON<{ messages?: StoredMessage[] }>(
    `/v1/conversations/${encodeURIComponent(conversationId)}/messages` +
      `?session_id=${encodeURIComponent(sessionId)}`,
  )
  return body.messages ?? []
}

export async function deleteConversation(conversationId: string): Promise<void> {
  const res = await fetch(`/v1/conversations/${encodeURIComponent(conversationId)}`, {
    method: 'DELETE',
    headers: headers(),
  })
  if (!res.ok) throw await toError(res)
}

export async function renameConversation(conversationId: string, name: string): Promise<void> {
  const res = await fetch(`/v1/conversations/${encodeURIComponent(conversationId)}`, {
    method: 'PATCH',
    headers: headers({ 'Content-Type': 'application/json' }),
    body: JSON.stringify({ name }),
  })
  if (!res.ok) throw await toError(res)
}

/**
 * Downloads a generated image from the gateway.
 *
 * The address M365 puts in an answer needs the designer token and a fileToken
 * header, so the gateway downloads it and serves it under a reference of its
 * own. That route is behind the API key, and an `<img>` element sends no
 * header, which is why the bytes are fetched here and shown as a blob.
 */
export async function fetchGeneratedImage(path: string): Promise<Blob> {
  const res = await fetch(path, { headers: headers() })
  if (!res.ok) throw await toError(res)
  return await res.blob()
}

/** What M365 Copilot is allowed to remember about the account. */
export interface Personalization {
  memory_enabled: boolean
  insights_from_history_enabled: boolean
  custom_instruction_enabled: boolean
  graph_content_enabled: boolean
  personalization_allowed_by_tenant: boolean
}

export async function fetchPersonalization(): Promise<Personalization> {
  return await getJSON<Personalization>('/v1/personalization')
}

export async function fetchQuota(): Promise<QuotaResponse | null> {
  try {
    return await getJSON<QuotaResponse>('/v1/quota')
  } catch {
    return null
  }
}

/**
 * Turns the account's Copilot memory on or off.
 *
 * This changes the operator's real M365 account, so it runs only when someone
 * moves the control. The gateway verifies the change upstream and answers with
 * the state the account reports, which is what the caller should render rather
 * than the value it asked for.
 */
export async function setMemoryEnabled(enabled: boolean): Promise<Personalization> {
  const res = await fetch('/v1/personalization', {
    method: 'PATCH',
    headers: headers({ 'Content-Type': 'application/json' }),
    body: JSON.stringify({ memory_enabled: enabled }),
  })
  if (!res.ok) throw await toError(res)
  return (await res.json()) as Personalization
}

export interface StreamDelta {
  content?: string
  reasoning?: string
  /**
   * A state of the turn the gateway reports while it waits, such as
   * `image_generating`. It arrives on an SSE comment, so it is not content and
   * never reaches a transcript.
   */
  notice?: string
}

/**
 * Streams one turn.
 *
 * The gateway answers with server-sent events, and EventSource cannot POST, so
 * the body is read and framed here. Keepalive comments carry no `data:` line
 * and fall through the parser untouched.
 */
export async function* streamChat(
  sessionId: string,
  model: string,
  content: string,
  signal: AbortSignal,
): AsyncGenerator<StreamDelta> {
  const res = await fetch('/v1/chat/completions', {
    method: 'POST',
    signal,
    headers: headers({
      'Content-Type': 'application/json',
      'X-Session-Id': sessionId,
    }),
    body: JSON.stringify({
      model,
      stream: true,
      messages: [{ role: 'user', content }],
    }),
  })
  if (!res.ok) throw await toError(res)
  if (!res.body) {
    throw new ApiError(res.status, 'no_body', 'The response carried no body', 'error.noBody')
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })

    let boundary = buffer.indexOf('\n\n')
    while (boundary >= 0) {
      const frame = buffer.slice(0, boundary)
      buffer = buffer.slice(boundary + 2)
      const delta = parseFrame(frame)
      if (delta === 'done') return
      if (delta) yield delta
      boundary = buffer.indexOf('\n\n')
    }
  }
}

/** The comment prefix the gateway reports a state of the turn on. */
const noticePrefix = ': notice '

/** Parses one SSE frame into a delta, 'done' for the terminator, or null. */
function parseFrame(frame: string): StreamDelta | 'done' | null {
  for (const line of frame.split('\n')) {
    // A notice rides on an SSE comment, which every other client ignores. The
    // keepalive comment falls through this and stays ignored here too.
    if (line.startsWith(noticePrefix)) {
      const notice = line.slice(noticePrefix.length).trim()
      if (notice) return { notice }
      continue
    }
    if (!line.startsWith('data:')) continue
    const payload = line.slice('data:'.length).trim()
    if (payload === '[DONE]') return 'done'
    if (!payload) continue

    let chunk: {
      error?: { code?: string; message?: string }
      choices?: { delta?: { content?: string; reasoning_content?: string } }[]
    }
    try {
      chunk = JSON.parse(payload)
    } catch {
      // A frame the parser cannot read is dropped rather than shown as text,
      // because rendering it would put transport noise into the answer.
      continue
    }

    // A stream that fails mid-turn reports it in the frame itself; the HTTP
    // status was already sent and cannot be changed.
    if (chunk.error) {
      // The gateway's own message is preferred and stays as it arrived; the
      // catalog key covers only the case where it sent none.
      throw new ApiError(
        502,
        chunk.error.code ?? 'stream_error',
        chunk.error.message ?? 'The stream failed',
        chunk.error.message ? undefined : 'error.streamFailed',
      )
    }

    const delta = chunk.choices?.[0]?.delta
    if (!delta) continue
    if (delta.content) return { content: delta.content }
    if (delta.reasoning_content) return { reasoning: delta.reasoning_content }
  }
  return null
}
