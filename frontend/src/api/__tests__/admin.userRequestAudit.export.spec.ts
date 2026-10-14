import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { post } }))

import { userRequestAuditAPI } from '@/api/admin/userRequestAudit'

describe('user request audit export API', () => {
  beforeEach(() => {
    post.mockReset()
    post.mockResolvedValue({ data: { id: 'job-1', status: 'queued' } })
  })

  it('keeps active filters when a selection is exported', async () => {
    await userRequestAuditAPI.export(
      { page: 1, page_size: 20, requested_model: 'claude-opus-4-1', group_id: 4, start_time: '2026-10-14T00:00:00.000Z' },
      'jsonl',
      [11, 12]
    )

    expect(post).toHaveBeenCalledWith('/admin/user-request-audit/export', {
      format: 'jsonl',
      requested_model: 'claude-opus-4-1',
      group_id: 4,
      start_time: '2026-10-14T00:00:00.000Z',
      conversation_ids: [11, 12]
    })
  })

  it('exports the filtered set without ids when nothing is selected', async () => {
    await userRequestAuditAPI.export({ page: 2, page_size: 50, requested_model: 'gpt-5.1-codex' }, 'json')

    expect(post).toHaveBeenCalledWith('/admin/user-request-audit/export', {
      format: 'json',
      requested_model: 'gpt-5.1-codex'
    })
  })
})
