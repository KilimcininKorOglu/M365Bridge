export interface ModelEntry {
  id: string
  display_name?: string
  capabilities?: {
    thinking?: { supported?: boolean }
    effort?: { supported?: boolean }
  }
}

// The field names come from the live RefreshNavPane response, not from a
// documented schema, so anything the interface does not read is left out.
export interface M365Conversation {
  conversationId: string
  chatName?: string
  updateTimeUtc?: number
  isMessageless?: boolean
  tone?: string
}

export interface SessionRecord {
  id: string
  conversation_id: string
  updated_at: number
}

// A row in the sidebar. It comes from the M365 conversation list, from the
// local session mappings, or from both when the two describe the same
// conversation.
export interface ConversationRow {
  sessionId: string
  conversationId: string
  title: string
  updatedAt: number
  /** True when the row exists only in M365 and has never been bound here. */
  remoteOnly: boolean
}

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
  thinking?: string
  model?: string
  createdAt?: number
  /** Set when the turn failed, so the row renders as an error instead of an answer. */
  error?: string
  /**
   * The gateway's notice token naming what the turn is waiting on, such as
   * `image_generating`. It comes from the stream rather than from a transcript,
   * so it is gone after a reload and after the first content arrives.
   */
  notice?: string
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  /**
   * A catalog key for failures this interface raises itself. A failure the
   * gateway reported carries none, because its message arrives already written
   * and in one language.
   */
  readonly key?: string

  constructor(status: number, code: string, message: string, key?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.key = key
  }
}

export interface ModelAllowance {
  daily?: number
  weekly?: number
}

export interface QuotaResponse {
  object: 'quota'
  available: boolean
  exhausted?: boolean
  used?: number
  max?: number
  headroom?: number
  allowances?: Record<string, ModelAllowance>
  raw_metering?: Record<string, number>
  detail?: string
}

