import { useCallback, useEffect, useRef, useState } from 'react'
import * as api from './api'
import { ApiError } from './types'
import type { ChatMessage, ConversationRow, ModelEntry, QuotaResponse } from './types'
import { apiKeyCookie, clearCookie, modelCookie, readCookie, writeCookie } from './cookies'
import { useI18n } from './i18n'
import { ApiKeyGate } from './components/ApiKeyGate'
import { ChatPane } from './components/ChatPane'
import { Sidebar } from './components/Sidebar'
import type { MemoryState } from './components/Sidebar'

/** How many session-only rows get a title fetched from their transcript. */
const titleHydrationLimit = 30

/**
 * A message for the user, held as the catalog key it came from rather than as
 * finished text. A notice raised before a language change would otherwise stay
 * on screen in the language it was raised in. An upstream failure has no key,
 * so it carries its text instead.
 */
type Notice = { key: string } | { text: string } | null

/**
 * The path prefix an open conversation takes in the address bar.
 *
 * The server knows this prefix too, in browserRoutes, so a reload or a pasted
 * link on one of these paths is answered with the document rather than a 404.
 */
const routePrefix = '/c/'

/** Returns the session the address bar names, or an empty string for none. */
function sessionFromLocation(): string {
  if (!location.pathname.startsWith(routePrefix)) {
    return ''
  }
  try {
    return decodeURIComponent(location.pathname.slice(routePrefix.length))
  } catch {
    // A hand-edited address can carry a percent sign that decodes to nothing.
    return ''
  }
}

/**
 * Points the address bar at a session without reloading the page.
 *
 * A push that lands on the address already shown is skipped, so returning
 * through the history does not add the entry it just left back onto the stack.
 */
function pushRoute(sessionID: string, replace = false): void {
  const url = sessionID ? routePrefix + encodeURIComponent(sessionID) : '/'
  if (url === location.pathname) {
    return
  }
  if (replace) {
    history.replaceState(null, '', url)
    return
  }
  history.pushState(null, '', url)
}

// A version 4 UUID built from getRandomValues, which carries no secure-context
// restriction of its own. The two masked bytes are the version and the variant.
function uuidFromRandomBytes(source: Crypto): string {
  const bytes = new Uint8Array(16)
  source.getRandomValues(bytes)
  const hex = Array.from(bytes, (byte, index) => {
    // Byte 6 carries the version nibble and byte 8 carries the variant bits.
    // The mapper reads each byte as an argument, because an indexed read of a
    // typed array is possibly undefined under noUncheckedIndexedAccess.
    if (index === 6) {
      return (((byte & 0x0f) | 0x40)).toString(16).padStart(2, '0')
    }
    if (index === 8) {
      return (((byte & 0x3f) | 0x80)).toString(16).padStart(2, '0')
    }
    return byte.toString(16).padStart(2, '0')
  }).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

// crypto.randomUUID exists only in a secure context, so a gateway reached over
// plain http on anything but localhost does not carry it and calling it throws.
// getRandomValues survives there and is still a real CSPRNG, so it is the first
// fallback. The timestamp form is the last resort for a runtime carrying
// neither, and it is the only branch that is not cryptographically random.
function newSessionId(): string {
  const source = globalThis.crypto
  if (typeof source?.randomUUID === 'function') {
    return `ui-${source.randomUUID()}`
  }
  if (typeof source?.getRandomValues === 'function') {
    return `ui-${uuidFromRandomBytes(source)}`
  }
  return `ui-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 11)}`
}

function firstLine(text: string): string {
  const line = text.trim().split('\n', 1)[0] ?? ''
  return line.length > 60 ? `${line.slice(0, 60)}…` : line
}

export function App() {
  const { t } = useI18n()
  // The cookie holds whatever credential this browser must send: an API key, or
  // the interface's password. Both travel in the same header.
  const [apiKey, setApiKeyState] = useState(() => readCookie(apiKeyCookie))
  const [authRequired, setAuthRequired] = useState(false)
  // Until the gateway says what it asks for, nothing is drawn: opening the
  // interface first and replacing it with a gate a moment later shows a
  // conversation list to someone who has not passed the gate yet.
  const [authMode, setAuthMode] = useState<api.AuthMode | null>(null)
  const [credentialRejected, setCredentialRejected] = useState(false)
  // Turns true once the gateway has nothing left to ask for. Every data call
  // waits on it, so a refused credential never loads a conversation list.
  const [unlocked, setUnlocked] = useState(false)

  const [models, setModels] = useState<ModelEntry[]>([])
  // The default is the id GET /v1/models advertises, not the registry key that
  // also resolves to it: the picker matches on the advertised id and would
  // otherwise show the stored value as one the catalog does not carry.
  const [model, setModel] = useState(() => readCookie(modelCookie) || 'gpt-5.5-reasoning')

  // The account's Copilot memory setting. Null until it has been read, and it
  // stays null when the read failed, so the control is absent rather than
  // showing a state it cannot vouch for.
  const [memory, setMemory] = useState<MemoryState | null>(null)
  const [quota, setQuota] = useState<QuotaResponse | null>(null)

  const [rows, setRows] = useState<ConversationRow[]>([])
  const [rowsLoaded, setRowsLoaded] = useState(false)
  const [activeId, setActiveId] = useState('')
  const [messages, setMessages] = useState<ChatMessage[]>([])

  const [remoteListFailed, setRemoteListFailed] = useState(false)
  const [transcriptsOff, setTranscriptsOff] = useState(false)
  const [notice, setNotice] = useState<Notice>(null)
  const [sending, setSending] = useState(false)
  // Set when the open conversation exists upstream but has no transcript here.
  // Reading it costs a page download and a walk of a serialization this project
  // does not control, so it waits for the user to ask.
  const [importable, setImportable] = useState<{ sessionId: string; conversationId: string } | null>(null)
  const [importing, setImporting] = useState(false)

  const abortRef = useRef<AbortController | null>(null)
  // The history listener reads the rows without re-subscribing every time they
  // change, which would otherwise drop and re-add the listener on each refresh.
  const rowsRef = useRef<ConversationRow[]>([])
  rowsRef.current = rows
  // The address bar is applied to the interface once, on the first load. After
  // that the interface is what writes it.
  const routeApplied = useRef(false)

  api.setApiKey(apiKey)

  const report = useCallback((err: unknown) => {
    if (err instanceof ApiError) {
      if (err.status === 401) {
        setAuthRequired(true)
        setNotice({ key: 'notice.authRequired' })
        return
      }
      if (err.key) {
        setNotice({ key: err.key })
        return
      }
      setNotice({ text: `${err.code}: ${err.message}` })
      return
    }
    if (err instanceof DOMException && err.name === 'AbortError') return
    setNotice({ text: String(err) })
  }, [])

  /**
   * Fills in titles for rows the M365 list did not name. The gateway stores no
   * title of its own, so the first thing the user said stands in for one.
   */
  const hydrateTitles = useCallback(async (merged: ConversationRow[]) => {
    const unnamed = merged.filter((row) => !row.title && row.sessionId).slice(0, titleHydrationLimit)
    for (const row of unnamed) {
      try {
        const stored = await api.loadMessages(row.sessionId)
        const first = stored.find((m) => m.role === 'user')
        if (!first) continue
        setRows((previous) =>
          previous.map((candidate) =>
            candidate.sessionId === row.sessionId && !candidate.title
              ? { ...candidate, title: firstLine(first.content) }
              : candidate,
          ),
        )
      } catch (err) {
        if (err instanceof ApiError && err.code === 'transcripts_disabled') {
          setTranscriptsOff(true)
          return
        }
        // One unreadable transcript must not stop the rest.
      }
    }
  }, [])

  /**
   * Builds the sidebar from both sources. The M365 list carries the names, the
   * local mappings carry the session ids, and a conversation present in both is
   * one row.
   */
  const refreshRows = useCallback(async () => {
    let sessions: Awaited<ReturnType<typeof api.listSessions>> = []
    try {
      sessions = await api.listSessions()
    } catch (err) {
      report(err)
      // There is no sidebar to open the address bar's conversation from, and
      // saying so beats leaving the interface waiting for rows that never come.
      setRowsLoaded(true)
      return
    }
    const sessionByConversation = new Map(sessions.map((s) => [s.conversation_id, s]))

    let merged: ConversationRow[] = []
    try {
      const conversations = await api.listConversations()
      setRemoteListFailed(false)
      merged = conversations
        .filter((c) => !c.isMessageless)
        .map((c) => {
          const session = sessionByConversation.get(c.conversationId)
          if (session) sessionByConversation.delete(c.conversationId)
          return {
            sessionId: session?.id ?? '',
            conversationId: c.conversationId,
            title: c.chatName?.trim() || t('conversation.unnamed'),
            updatedAt: c.updateTimeUtc ?? session?.updated_at ?? 0,
            remoteOnly: !session,
          }
        })
    } catch {
      // Listing M365 conversations needs browser cookies. Without them the
      // local mappings are the whole sidebar, and saying so beats an empty one.
      setRemoteListFailed(true)
    }

    for (const session of sessionByConversation.values()) {
      merged.push({
        sessionId: session.id,
        conversationId: session.conversation_id,
        title: '',
        updatedAt: session.updated_at,
        remoteOnly: false,
      })
    }

    merged.sort((a, b) => b.updatedAt - a.updatedAt)
    setRows((previous) => {
      // A conversation that has not had its first turn yet exists only here, so
      // it is carried over. Once its first turn creates the upstream
      // conversation the merged list describes the same thing, and keeping the
      // local copy too would show one conversation as two rows.
      const known = new Set(merged.map((row) => row.sessionId))
      const unsent = previous.filter((row) => !row.conversationId && !known.has(row.sessionId))
      return [...unsent, ...merged]
    })
    setRowsLoaded(true)
    void hydrateTitles(merged)
    // t is a dependency because an unnamed conversation carries a translated
    // placeholder for a title: changing the language relabels those rows.
  }, [hydrateTitles, report, t])

  // Asks the gateway what it wants before anything is drawn, then tries the
  // credential this browser already holds. A stored credential the gateway no
  // longer accepts is cleared rather than retried on every call.
  useEffect(() => {
    let cancelled = false

    async function resolveAuth() {
      let mode: api.AuthMode = 'none'
      try {
        mode = await api.fetchAuthMode()
      } catch {
        // A gateway that cannot answer this cannot serve a conversation either.
        // Treating it as open lets the ordinary error path report the failure
        // instead of holding the interface behind a gate nobody can pass.
      }
      if (cancelled) return
      setAuthMode(mode)

      if (mode === 'none') {
        setUnlocked(true)
        return
      }

      const stored = readCookie(apiKeyCookie)
      if (!stored) return

      try {
        if (await api.verifyCredential(stored)) {
          if (!cancelled) setUnlocked(true)
          return
        }
      } catch {
        // Fall through to the gate; the user can offer another credential.
      }
      if (cancelled) return
      clearCookie(apiKeyCookie)
      setApiKeyState('')
      setCredentialRejected(true)
    }

    void resolveAuth()
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    if (!unlocked) return
    api.listModels().then(setModels).catch(report)
  }, [report, unlocked])

  useEffect(() => {
    if (!unlocked) return
    void refreshRows()
  }, [refreshRows, unlocked])

  useEffect(() => {
    if (!unlocked) return
    api
      .fetchPersonalization()
      .then((flags) =>
        setMemory({
          enabled: flags.memory_enabled,
          allowedByTenant: flags.personalization_allowed_by_tenant,
          saving: false,
        }),
      )
      // The gateway works without this setting, so a failed read hides the
      // control and reports itself rather than blocking the interface.
      .catch(report)
  }, [report, unlocked])

  const refreshQuota = useCallback(async () => {
    try {
      const q = await api.fetchQuota()
      if (q && q.available) {
        setQuota(q)
      }
    } catch {
      // Quota is best-effort and does not report an error banner.
    }
  }, [])

  useEffect(() => {
    if (!unlocked) return
    void refreshQuota()
  }, [refreshQuota, unlocked])

  const changeMemory = useCallback(
    (enabled: boolean) => {
      setMemory((previous) => (previous ? { ...previous, saving: true } : previous))
      api
        .setMemoryEnabled(enabled)
        // The gateway verifies the change upstream, so what it answers is the
        // state to render, not the value that was asked for.
        .then((flags) =>
          setMemory({
            enabled: flags.memory_enabled,
            allowedByTenant: flags.personalization_allowed_by_tenant,
            saving: false,
          }),
        )
        .catch((err) => {
          // A failed change must not leave the control showing the value it
          // failed to set, so the read decides what is true.
          report(err)
          api
            .fetchPersonalization()
            .then((flags) =>
              setMemory({
                enabled: flags.memory_enabled,
                allowedByTenant: flags.personalization_allowed_by_tenant,
                saving: false,
              }),
            )
            .catch(() => setMemory(null))
        })
    },
    [report],
  )

  const openRow = useCallback(
    async (row: ConversationRow) => {
      setNotice(null)
      setImportable(null)
      let sessionId = row.sessionId
      if (!sessionId) {
        // The conversation exists upstream but nothing here points at it, so it
        // gets a session id before it can be continued.
        sessionId = newSessionId()
        try {
          await api.bindSession(sessionId, row.conversationId)
        } catch (err) {
          report(err)
          return
        }
        setRows((previous) =>
          previous.map((candidate) =>
            candidate.conversationId === row.conversationId
              ? { ...candidate, sessionId, remoteOnly: false }
              : candidate,
          ),
        )
      }
      setActiveId(sessionId)
      pushRoute(sessionId)

      try {
        const stored = await api.loadMessages(sessionId)
        setMessages(
          stored.map((entry) => ({
            role: entry.role,
            content: entry.content,
            thinking: entry.thinking,
            model: entry.model,
            createdAt: entry.created_at,
          })),
        )
        if (stored.length === 0 && row.conversationId) {
          setImportable({ sessionId, conversationId: row.conversationId })
          setNotice({ key: 'notice.foreignConversation' })
        }
      } catch (err) {
        if (err instanceof ApiError && err.code === 'transcripts_disabled') {
          setTranscriptsOff(true)
          setMessages([])
          return
        }
        report(err)
      }
    },
    [report],
  )

  /** Pulls the history of the open conversation from M365 and stores it here. */
  const importHistory = useCallback(async () => {
    if (!importable || importing) return
    setImporting(true)
    setNotice(null)
    try {
      const stored = await api.importHistory(importable.conversationId, importable.sessionId)
      setMessages(
        stored.map((entry) => ({
          role: entry.role,
          content: entry.content,
          thinking: entry.thinking,
          model: entry.model,
          createdAt: entry.created_at,
        })),
      )
      setImportable(null)
      void refreshRows()
    } catch (err) {
      report(err)
    } finally {
      setImporting(false)
    }
  }, [importable, importing, refreshRows, report])

  const startNew = useCallback(() => {
    const sessionId = newSessionId()
    setRows((previous) => [
      {
        sessionId,
        conversationId: '',
        title: t('conversation.new'),
        updatedAt: Date.now() / 1000,
        remoteOnly: false,
      },
      ...previous,
    ])
    setActiveId(sessionId)
    pushRoute(sessionId)
    setMessages([])
    setNotice(null)
    setImportable(null)
  }, [t])

  const removeRow = useCallback(
    async (row: ConversationRow) => {
      // A conversation with no upstream id was never sent, so there is nothing
      // to delete anywhere but here.
      if (row.conversationId && row.sessionId) {
        try {
          await api.deleteSession(row.sessionId, remoteListFailed)
        } catch (err) {
          report(err)
          return
        }
      } else if (row.conversationId) {
        try {
          await api.deleteConversation(row.conversationId)
        } catch (err) {
          report(err)
          return
        }
      }
      setRows((previous) => previous.filter((candidate) => candidate !== row))
      if (row.sessionId === activeId) {
        setActiveId('')
        setMessages([])
        // The address named the conversation that was just deleted, and a
        // reload on that address would look for something that is gone.
        pushRoute('', true)
      }
      void refreshRows()
    },
    [activeId, refreshRows, remoteListFailed, report],
  )

  const renameRow = useCallback(
    async (row: ConversationRow, name: string) => {
      if (!row.conversationId) {
        setNotice({ key: 'notice.renameBeforeSend' })
        return
      }
      try {
        await api.renameConversation(row.conversationId, name)
      } catch (err) {
        report(err)
        return
      }
      setRows((previous) =>
        previous.map((candidate) => (candidate === row ? { ...candidate, title: name } : candidate)),
      )
    },
    [report],
  )

  const send = useCallback(
    async (text: string) => {
      let sessionId = activeId
      if (!sessionId) {
        sessionId = newSessionId()
        setRows((previous) => [
          { sessionId, conversationId: '', title: firstLine(text), updatedAt: Date.now() / 1000, remoteOnly: false },
          ...previous,
        ])
        setActiveId(sessionId)
        pushRoute(sessionId)
      }

      setNotice(null)
      setSending(true)
      setMessages((previous) => [
        ...previous,
        { role: 'user', content: text },
        { role: 'assistant', content: '', model },
      ])

      const controller = new AbortController()
      abortRef.current = controller

      try {
        for await (const delta of api.streamChat(sessionId, model, text, controller.signal)) {
          setMessages((previous) => {
            const next = [...previous]
            const last = next[next.length - 1]
            if (!last || last.role !== 'assistant') return previous
            next[next.length - 1] = {
              ...last,
              content: delta.content ? last.content + delta.content : last.content,
              thinking: delta.reasoning ? (last.thinking ?? '') + delta.reasoning : last.thinking,
              // A notice covers the wait, so the first content replaces it.
              // Nothing has to be taken back: it was never part of the answer.
              notice: delta.content ? undefined : (delta.notice ?? last.notice),
            }
            return next
          })
        }
      } catch (err) {
        const message =
          err instanceof ApiError ? `${err.code}: ${err.message}` : String(err)
        if (!(err instanceof DOMException && err.name === 'AbortError')) {
          setMessages((previous) => {
            const next = [...previous]
            const last = next[next.length - 1]
            if (last && last.role === 'assistant' && !last.content) {
              next[next.length - 1] = { ...last, error: message }
              return next
            }
            return previous
          })
        }
        report(err)
      } finally {
        abortRef.current = null
        setSending(false)
        // The first turn is what creates the upstream conversation, so the
        // sidebar only learns its id once the turn is over.
        void refreshRows()
        void refreshQuota()
      }
    },
    [activeId, model, refreshQuota, refreshRows, report],
  )

  const stop = useCallback(() => {
    abortRef.current?.abort()
  }, [])

  /** Clears the pane without naming another conversation. */
  const closeConversation = useCallback(() => {
    setActiveId('')
    setMessages([])
    setNotice(null)
    setImportable(null)
  }, [])

  // Opens the conversation the address bar names, once the sidebar has rows to
  // find it among. This is what makes a reload and a shared link land on the
  // conversation rather than on an empty pane.
  useEffect(() => {
    if (!rowsLoaded || routeApplied.current) return
    routeApplied.current = true

    const wanted = sessionFromLocation()
    if (!wanted) return

    const row = rows.find((candidate) => candidate.sessionId === wanted)
    if (row) {
      void openRow(row)
      return
    }
    // The address names a conversation this gateway no longer holds. Replacing
    // rather than pushing keeps a dead address out of the history.
    pushRoute('', true)
  }, [openRow, rows, rowsLoaded])

  // The back and forward buttons move between the conversations that were
  // opened, so they change what the pane shows rather than leaving the page.
  useEffect(() => {
    function onPopState() {
      const wanted = sessionFromLocation()
      if (!wanted) {
        closeConversation()
        return
      }
      const row = rowsRef.current.find((candidate) => candidate.sessionId === wanted)
      if (row) void openRow(row)
    }
    window.addEventListener('popstate', onPopState)
    return () => window.removeEventListener('popstate', onPopState)
  }, [closeConversation, openRow])

  const chooseModel = useCallback((next: string) => {
    setModel(next)
    writeCookie(modelCookie, next)
  }, [])

  // Checks the credential before storing it, so a wrong password is refused at
  // the gate rather than accepted and then failing on every call behind it.
  const saveCredential = useCallback(
    async (credential: string) => {
      try {
        if (!(await api.verifyCredential(credential))) {
          setCredentialRejected(true)
          return
        }
      } catch (err) {
        report(err)
        return
      }
      setApiKeyState(credential)
      api.setApiKey(credential)
      writeCookie(apiKeyCookie, credential)
      setCredentialRejected(false)
      setAuthRequired(false)
      setUnlocked(true)
      setNotice(null)
    },
    [report],
  )

  // Nothing is drawn until the gateway has said what it asks for. Opening the
  // interface first and replacing it with a gate a moment later would show a
  // conversation list to someone who has not passed the gate yet.
  if (authMode === null) {
    return null
  }

  // Shown while the interface is locked, and again whenever a data call is
  // refused: a stored credential that no longer works has to be replaceable,
  // not a lockout.
  if (!unlocked || authRequired) {
    return (
      <ApiKeyGate
        mode={authMode === 'none' ? 'api_key' : authMode}
        rejected={credentialRejected}
        onSubmit={saveCredential}
      />
    )
  }

  // A row that M365 lists but nothing here has bound carries an empty session
  // id, so an empty activeId would match the first such row and title the pane
  // after a conversation the user never opened.
  const activeRow = activeId ? rows.find((row) => row.sessionId === activeId) : undefined

  const noticeText = notice === null ? '' : 'key' in notice ? t(notice.key) : notice.text

  return (
    <div className="layout">
      <Sidebar
        rows={rows}
        activeId={activeId}
        remoteListFailed={remoteListFailed}
        memory={memory}
        quota={quota}
        onOpen={openRow}
        onNew={startNew}
        onDelete={removeRow}
        onRename={renameRow}
        onMemoryChange={changeMemory}
      />
      <ChatPane
        messages={messages}
        models={models}
        model={model}
        sending={sending}
        notice={noticeText}
        transcriptsOff={transcriptsOff}
        title={activeRow?.title ?? ''}
        canImportHistory={importable !== null}
        importing={importing}
        onImportHistory={importHistory}
        onModel={chooseModel}
        onSend={send}
        onStop={stop}
      />
    </div>
  )
}
