import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import UserRequestAuditTranscript from '../UserRequestAuditTranscript.vue'
import type { UserRequestAuditConversation } from '@/api/admin/userRequestAudit'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        const substituted = key.replace(/\{(\w+)\}/g, (_, token: string) => String(params?.[token] ?? ''))
        if (params && !key.includes('{')) return `${substituted} ${Object.values(params).join(',')}`
        return substituted
      },
    }),
  }
})

const conversation = (overrides: Partial<UserRequestAuditConversation>): UserRequestAuditConversation => ({
  id: 1,
  user_label: 'u',
  request_count: 20,
  request_start: 1,
  request_end: 20,
  messages: [{ role: 'user', content: 'hello', kind: 'text', request_id: 1 }],
  requests: [],
  ...overrides,
})

const mountReader = (doc: UserRequestAuditConversation) => mount(UserRequestAuditTranscript, {
  props: { conversation: doc, loading: false, error: '', loadingMore: false, windowSize: 20 },
})

describe('UserRequestAuditTranscript conversation paging', () => {
  it('offers the next window only while requests remain and caps the final window', async () => {
    const partial = mountReader(conversation({ request_count: 45, request_start: 1, request_end: 20 }))
    expect(partial.text()).toContain('admin.userRequestAudit.windowProgress')
    const button = partial.get('button')
    expect(button.text()).toContain('20') // min(window size 20, 45 - 20 remaining)
    await button.trigger('click')
    expect(partial.emitted('load-more')).toHaveLength(1)

    const final = mountReader(conversation({ request_count: 45, request_start: 21, request_end: 45 }))
    expect(final.findAll('button').some(button => button.text().includes('loadNextRequests'))).toBe(false)
    await final.get('button').trigger('click')
    expect(final.emitted('load-previous')).toHaveLength(1)
  })

  it('bounds mounted messages and preserves forward and backward access', async () => {
    const messages = Array.from({ length: 43 }, (_, index) => ({ role: 'user', content: `message-${index}`, kind: 'text', request_id: 1 }))
    const reader = mountReader(conversation({ messages }))
    const click = async (key: string) => reader.findAll('button').find(button => button.text().includes(key))!.trigger('click')
    expect(reader.findAll('article')).toHaveLength(20)
    expect(reader.findAll('article').map(block => block.get('pre').text())).toEqual(messages.slice(0, 20).map(message => message.content))
    await click('nextMessages')
    expect(reader.findAll('article').map(block => block.get('pre').text())).toEqual(messages.slice(20, 40).map(message => message.content))
    await click('nextMessages')
    expect(reader.findAll('article').map(block => block.get('pre').text())).toEqual(messages.slice(40).map(message => message.content))
    await click('previousMessages')
    expect(reader.findAll('article')).toHaveLength(20)
    expect(reader.findAll('article')[0]!.text()).toContain('message-20')
  })

  it('keeps every text character across bounded segments without splitting emoji', async () => {
    const text = 'a'.repeat(16383) + '😀' + 'b'.repeat(17000)
    const reader = mountReader(conversation({ messages: [{ role: 'assistant', content: text, kind: 'text', request_id: 1 }] }))
    const parts: string[] = []
    for (let page = 0; page < 3; page++) {
      const part = reader.get('article pre').text()
      expect(part.length).toBeLessThanOrEqual(16385)
      expect(part).not.toMatch(/^[\uDC00-\uDFFF]|[\uD800-\uDBFF]$/)
      parts.push(part)
      if (page < 2) await reader.findAll('button').find(button => button.text().includes('nextText'))!.trigger('click')
    }
    expect(parts.join('')).toBe(text)
    await reader.findAll('button').find(button => button.text().includes('previousText'))!.trigger('click')
    expect(reader.get('article pre').text()).toBe(parts[1])
  })

  it('does not mount collapsed reasoning until opened', async () => {
    const reader = mountReader(conversation({ messages: [{ role: 'assistant', content: 'private reasoning', kind: 'think', request_id: 1 }] }))
    expect(reader.find('pre').exists()).toBe(false)
    const details = reader.get('details')
    ;(details.element as HTMLDetailsElement).open = true
    await details.trigger('toggle')
    expect(reader.get('pre').text()).toBe('private reasoning')
  })
})
