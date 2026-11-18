import 'fake-indexeddb/auto'
import { expect, it, vi } from 'vitest'
import { RealtimeSession } from './realtimeSession'
import * as transport from './realtimeTransport'
import { useCollaborationStore } from '../stores/collaborationStore'
import type { RealtimeMessage } from '../types/protocol'

it('projects pending edits once, ignores duplicate ACKs and reconnects on a version gap', async () => {
  const sent: RealtimeMessage[] = []
  const close = vi.fn()
  let receive!: (message: RealtimeMessage) => void
  const connect = vi.spyOn(transport, 'connectRealtimeTransport').mockImplementation(async (_, __, callbacks) => {
    receive = callbacks.message
    return { kind: 'websocket', send: m => sent.push(m), close }
  })
  const cursorChanged = vi.fn()
  const session = new RealtimeSession(crypto.randomUUID(), 'alice', { cursorChanged })
  const deliver = async (m: RealtimeMessage) => {
    receive(m)
    await session.flushEdits()
  }
  try {
    await session.start()
    await deliver({ type: 'auth_ok', local_cursor_transform: true })
    await deliver({ type: 'joined', content: 'ab', version: 0, role: 'write' })
    await deliver({ type: 'cursor_changed', cursor: { user_id: 'bob', version: 0, anchor: 2, head: 1, sequence: 1 } })
    await session.updateContent('🙂ab')
    expect(cursorChanged.mock.lastCall?.[0]).toMatchObject({ anchor: 3, head: 2 })
    const edit = sent.find(m => m.type === 'edit')
    expect(edit?.request_id).toBeTruthy()
    const ack: RealtimeMessage = { type: 'edit_applied', version: 1, operation: edit!.operation, request_id: edit!.request_id }
    await deliver(ack)
    await deliver(ack)
    expect(cursorChanged.mock.lastCall?.[0]).toMatchObject({ anchor: 3, head: 2, version: 1 })
    expect(useCollaborationStore.getState()).toMatchObject({ content: '🙂ab', version: 1, pendingCount: 0 })
    await deliver({ ...ack, version: 3 })
    expect(close).toHaveBeenCalledWith('edit version gap')
  } finally {
    session.stop()
    connect.mockRestore()
    useCollaborationStore.getState().reset()
  }
})
