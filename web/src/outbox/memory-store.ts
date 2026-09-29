/* The in-memory `OutboxStore`: tests, SSR, and any context with no IndexedDB.
 *
 * It keeps the one property the outbox relies on — a write is visible to the
 * next read once its promise resolves — and copies on the way in and out, so
 * the outbox can never mutate a stored record by holding a reference to it.
 * It is not durable, and nothing that must survive a reload may use it.
 */

import type { Uuid } from '@/wire/generated'
import type { OutboxEntry, OutboxStore } from './types'

export class MemoryOutboxStore implements OutboxStore {
  private readonly rows = new Map<Uuid, OutboxEntry>()

  constructor(seed: OutboxEntry[] = []) {
    for (const e of seed) this.rows.set(e.clientId, structuredClone(e))
  }

  async add(entry: Omit<OutboxEntry, 'order'>): Promise<OutboxEntry> {
    // Synchronous from read to write, which is this store's whole transaction.
    let highest = 0
    for (const e of this.rows.values()) {
      if (e.accountId === entry.accountId && e.order > highest) highest = e.order
    }
    const stored = { ...structuredClone(entry), order: highest + 1 } as OutboxEntry
    this.rows.set(stored.clientId, stored)
    return structuredClone(stored)
  }

  async put(entry: OutboxEntry): Promise<void> {
    this.rows.set(entry.clientId, structuredClone(entry))
  }

  async update(clientId: Uuid, mutate: (e: OutboxEntry) => void): Promise<OutboxEntry | undefined> {
    const held = this.rows.get(clientId)
    if (!held) return undefined
    const next = structuredClone(held)
    mutate(next)
    this.rows.set(clientId, next)
    return structuredClone(next)
  }

  async delete(clientId: Uuid): Promise<void> {
    this.rows.delete(clientId)
  }

  async list(): Promise<OutboxEntry[]> {
    return [...this.rows.values()].map((e) => structuredClone(e)).sort(byOrder)
  }

  close(): void {}
}

export const byOrder = (a: OutboxEntry, b: OutboxEntry) =>
  a.accountId === b.accountId ? a.order - b.order : a.accountId < b.accountId ? -1 : 1
