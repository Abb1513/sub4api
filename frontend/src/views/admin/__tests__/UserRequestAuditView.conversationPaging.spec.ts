import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import UserRequestAuditView from '../UserRequestAuditView.vue'
import type { UserRequestAudit, UserRequestAuditConversation } from '@/api/admin/userRequestAudit'

const { list, getConversationWindow, showError } = vi.hoisted(() => ({
  list: vi.fn(),
  getConversationWindow: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    userRequestAudit: {
      list,
      getConversationWindow,
      getGroupStats: vi.fn(async () => []),
      getModels: vi.fn(async () => []),
      getStatus: vi.fn(async () => ({ total_bytes: 0, file_count: 0, disk_total_bytes: 0, disk_free_bytes: 0, recording_paused: false })),
      getConfig: vi.fn(async () => ({ retention_days: 7, cleanup_interval_hours: 24, group_ids: [] })),
      getExports: vi.fn(async () => []),
    },
    groups: { getAllIncludingInactive: vi.fn(async () => []) },
  },
}))

vi.mock('@/stores', () => ({ useAppStore: () => ({ showError }) }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string, params?: Record<string, string | number>) => (params ? `${key} ${Object.values(params).join(',')}` : key) }) }
})

const record: UserRequestAudit = {
  id: 500, created_at: '2026-10-14T00:00:00Z', updated_at: '2026-10-14T00:00:00Z', protocol: 'openai_responses',
  endpoint: '/v1/responses', status: 'completed', request_count: 45,
}

const segment = (start: number, end: number, total: number): UserRequestAuditConversation => ({
  id: 500,
  user_label: 'owner',
  request_count: total,
  request_start: start,
  request_end: end,
  messages: [{ role: 'user', content: `segment-${start}-${end}`, kind: 'text', request_id: start }],
  requests: [{ id: start } as UserRequestAudit],
})

const mountView = () => mount(UserRequestAuditView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
      Pagination: true, Select: true, BaseDialog: true, ConfirmDialog: true, Icon: true,
    },
  },
})

beforeEach(() => {
  vi.clearAllMocks()
  list.mockResolvedValue({ items: [record], total: 1 })
})

describe('UserRequestAuditView conversation reader paging', () => {
  it('requests only the first request window when a conversation is opened', async () => {
    getConversationWindow.mockResolvedValue(segment(1, 20, 45))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('button').find(entry => entry.text().includes('admin.userRequestAudit.readConversation'))!.trigger('click')
    await flushPromises()
    expect(getConversationWindow).toHaveBeenCalledWith(500, 0, 20, expect.anything())
    expect(wrapper.text()).toContain('segment-1-20')
  })

  it('replaces the next window and can return to earlier content', async () => {
    getConversationWindow
      .mockResolvedValueOnce(segment(1, 20, 45))
      .mockResolvedValueOnce(segment(21, 40, 45))
      .mockResolvedValueOnce(segment(1, 20, 45))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('button').find(entry => entry.text().includes('admin.userRequestAudit.readConversation'))!.trigger('click')
    await flushPromises()
    const more = wrapper.findAll('button').find(entry => entry.text().includes('admin.userRequestAudit.loadNextRequests'))
    expect(more).toBeTruthy()
    await more!.trigger('click')
    await flushPromises()
    expect(getConversationWindow).toHaveBeenLastCalledWith(500, 20, 20, expect.anything())
    const text = wrapper.text()
    expect(text).toContain('segment-21-40')
    expect(text).not.toContain('segment-1-20')
    await wrapper.findAll('button').find(entry => entry.text().includes('admin.userRequestAudit.previousRequests'))!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('segment-1-20')
    expect(wrapper.text()).not.toContain('segment-21-40')
  })

  it('restarts from the beginning when the conversation shifted underneath the reader', async () => {
    getConversationWindow
      .mockResolvedValueOnce(segment(1, 20, 45))
      .mockResolvedValueOnce(segment(31, 45, 45)) // gap: retention removed requests mid-read
      .mockResolvedValueOnce(segment(1, 20, 55))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.findAll('button').find(entry => entry.text().includes('admin.userRequestAudit.readConversation'))!.trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(entry => entry.text().includes('admin.userRequestAudit.loadNextRequests'))!.trigger('click')
    await flushPromises()
    expect(getConversationWindow).toHaveBeenCalledTimes(3)
    expect(getConversationWindow).toHaveBeenLastCalledWith(500, 0, 20, expect.anything())
    expect(wrapper.text()).toContain('segment-1-20')
    expect(wrapper.text()).not.toContain('segment-31-45')
  })
})
