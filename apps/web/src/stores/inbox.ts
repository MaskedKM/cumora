// stores/inbox —— #264 人侧 Inbox 分级:条目 + 未读计数 + 按 type 静音。
// WS inbox.new 由 NotificationToasts 订阅(弹条分级在那边),本店只管
// 列表/计数/静音的拉取与操作;徽标(actionRequired+attention 未读)由
// Rail 消费。
import { create } from 'zustand'
import { api, type ApiInboxItem, type ApiInboxResponse } from '@/api/client'

interface InboxState {
  loaded: boolean
  items: ApiInboxItem[]
  counts: { actionRequired: number; attention: number; info: number }
  mutedTypes: string[]
  load: () => Promise<void>
  markRead: (id: string) => Promise<void>
  markAllRead: () => Promise<void>
  setMutes: (types: string[]) => Promise<void>
}

export const useInbox = create<InboxState>((set, get) => ({
  loaded: false,
  items: [],
  counts: { actionRequired: 0, attention: 0, info: 0 },
  mutedTypes: [],
  load: async () => {
    try {
      const res = await api.getInbox()
      set({ loaded: true, items: res.items, counts: res.counts, mutedTypes: res.mutedTypes })
    } catch {
      // 拉取失败保旧值;徽标静默
    }
  },
  markRead: async (id) => {
    const prevItems = get().items
    const prevCounts = get().counts
    set({ items: prevItems.map((it) => (it.id === id ? { ...it, read: true } : it)) })
    try {
      await api.markInboxItemRead(id)
    } catch {
      set({ items: prevItems, counts: prevCounts })
      return
    }
    get().load()
  },
  markAllRead: async () => {
    const prevItems = get().items
    const prevCounts = get().counts
    set({
      items: prevItems.map((it) => ({ ...it, read: true })),
      counts: { actionRequired: 0, attention: 0, info: 0 },
    })
    try {
      await api.markAllInboxRead()
    } catch {
      set({ items: prevItems, counts: prevCounts })
      return
    }
    get().load()
  },
  setMutes: async (types) => {
    const prev = get().mutedTypes
    set({ mutedTypes: types })
    try {
      await api.setInboxMutes(types)
    } catch {
      set({ mutedTypes: prev })
    }
  },
}))
