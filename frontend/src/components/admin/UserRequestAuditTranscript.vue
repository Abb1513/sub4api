<template>
  <section class="space-y-3" :aria-label="t('admin.userRequestAudit.conversationReader')">
    <div v-if="loading" class="rounded-lg border border-gray-200 p-6 text-center text-sm text-gray-500 dark:border-dark-700">
      {{ t('common.loading') }}
    </div>
    <div v-else-if="error" class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-500/30 dark:bg-red-500/10 dark:text-red-300" role="alert">
      <p>{{ error }}</p>
      <button type="button" class="btn btn-secondary mt-3" @click="retry">{{ t('admin.userRequestAudit.retryConversation') }}</button>
    </div>
    <div v-else-if="!conversation" class="rounded-lg border border-gray-200 p-6 text-center text-sm text-gray-500 dark:border-dark-700">
      {{ t('admin.userRequestAudit.noConversation') }}
    </div>
    <div v-else class="space-y-3">
      <div v-if="!conversation.messages.length" class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-200">
        {{ t('admin.userRequestAudit.noPlaintextCaptured') }}
      </div>
      <div v-else-if="!hasThink" class="rounded-lg border border-gray-200 bg-gray-50 p-3 text-xs text-gray-600 dark:border-dark-700 dark:bg-dark-800/60 dark:text-dark-300">
        {{ t('admin.userRequestAudit.noThinkCaptured') }}
      </div>
      <div v-if="hasIncomplete" class="rounded-lg border-2 border-amber-300 bg-amber-50 p-3 text-sm font-medium text-amber-900 dark:border-amber-500/60 dark:bg-amber-500/10 dark:text-amber-100" role="status">
        {{ t('admin.userRequestAudit.incompleteWarning') }}
      </div>

      <div class="max-h-[min(70vh,900px)] space-y-3 overflow-y-auto pr-1" tabindex="0">
        <article
          v-for="(message, index) in conversation.messages"
          :key="`${message.request_id}-${index}`"
          class="rounded-lg border p-4 shadow-sm"
          :class="blockClass(message)"
        >
          <header class="mb-2 flex flex-wrap items-center gap-2 text-xs">
            <span class="font-semibold uppercase tracking-wide">{{ blockLabel(message) }}</span>
            <span v-if="message.request_id" class="text-gray-500 dark:text-dark-400">#{{ message.request_id }}</span>
            <span v-if="message.tool_name" class="rounded bg-gray-200 px-2 py-0.5 font-mono dark:bg-dark-700">{{ message.tool_name }}</span>
            <span v-if="message.call_id" class="break-all font-mono text-gray-500 dark:text-dark-400">{{ message.call_id }}</span>
            <span v-if="message.incomplete" class="rounded bg-amber-100 px-2 py-0.5 text-amber-800 dark:bg-amber-500/20 dark:text-amber-200">{{ t('admin.userRequestAudit.incomplete') }}</span>
          </header>

          <details v-if="isThink(message)" class="group">
            <summary class="cursor-pointer select-none text-sm font-medium">
              {{ t('admin.userRequestAudit.think') }}
              <span class="ml-1 text-xs font-normal text-gray-500 group-open:hidden">{{ t('admin.userRequestAudit.expand') }}</span>
            </summary>
            <pre class="mt-3 max-h-96 overflow-auto whitespace-pre-wrap break-words font-mono text-xs leading-5">{{ message.reasoning || message.content || t('admin.userRequestAudit.noPlaintextCaptured') }}</pre>
          </details>
          <details v-else-if="isCollapsed(message)" class="group">
            <summary class="cursor-pointer select-none text-sm font-medium">
              {{ blockLabel(message) }}
              <span class="ml-1 text-xs font-normal text-gray-500 group-open:hidden">{{ t('admin.userRequestAudit.expand') }}</span>
            </summary>
            <pre class="mt-3 max-h-96 overflow-auto whitespace-pre-wrap break-words font-mono text-xs leading-5">{{ message.content || t('admin.userRequestAudit.noPlaintextCaptured') }}</pre>
          </details>
          <template v-else-if="isTool(message)">
            <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded bg-gray-950/5 p-3 font-mono text-xs leading-5 dark:bg-black/20">{{ message.content || t('admin.userRequestAudit.noPlaintextCaptured') }}</pre>
          </template>
          <div v-else-if="isAssistant(message) && message.content" class="conversation-markdown prose prose-sm max-w-none break-words dark:prose-invert" v-html="renderAssistant(message.content)"></div>
          <pre v-else-if="!isCollapsed(message)" class="whitespace-pre-wrap break-words font-mono text-xs leading-5">{{ message.content || t('admin.userRequestAudit.noPlaintextCaptured') }}</pre>
        </article>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import type { UserRequestAuditConversation, UserRequestAuditMessage } from '@/api/admin/userRequestAudit'

const props = defineProps<{
  conversation: UserRequestAuditConversation | null
  loading: boolean
  error: string
}>()

const emit = defineEmits<{ retry: [] }>()

const { t } = useI18n()
const hasThink = computed(() => Boolean(props.conversation?.messages.some(message => isThink(message))))
const hasIncomplete = computed(() => Boolean(props.conversation?.messages.some(message => message.incomplete)))

function retry(): void {
  emit('retry')
}

function isThink(message: UserRequestAuditMessage): boolean {
  return message.kind === 'think' || Boolean(message.reasoning)
}

function isTool(message: UserRequestAuditMessage): boolean {
  return message.kind === 'tool_call' || message.kind === 'tool_result' || message.role === 'tool'
}

function isAssistant(message: UserRequestAuditMessage): boolean {
  return message.role.toLowerCase() === 'assistant' && !isTool(message)
}

function isCollapsed(message: UserRequestAuditMessage): boolean {
  const role = message.role.toLowerCase()
  return role === 'system' || role === 'developer' || role === 'tools-schema' || message.kind === 'metadata'
}

function blockLabel(message: UserRequestAuditMessage): string {
  if (isThink(message)) return t('admin.userRequestAudit.think')
  if (message.kind === 'tool_call') return t('admin.userRequestAudit.toolCall')
  if (message.kind === 'tool_result') return t('admin.userRequestAudit.toolResult')
  const role = message.role.toLowerCase()
  if (role === 'user') return t('admin.userRequestAudit.user')
  if (role === 'system') return t('admin.userRequestAudit.system')
  if (role === 'assistant') return t('admin.userRequestAudit.assistant')
  return message.role || t('admin.userRequestAudit.message')
}

function blockClass(message: UserRequestAuditMessage): string {
  if (isThink(message)) return 'border-violet-200 bg-violet-50/70 dark:border-violet-500/30 dark:bg-violet-500/10'
  if (isTool(message)) return 'border-sky-200 bg-sky-50/70 dark:border-sky-500/30 dark:bg-sky-500/10'
  if (message.role.toLowerCase() === 'user') return 'border-blue-200 bg-blue-50/70 dark:border-blue-500/30 dark:bg-blue-500/10'
  if (message.role.toLowerCase() === 'system') return 'border-gray-300 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'
  return 'border-emerald-200 bg-emerald-50/70 dark:border-emerald-500/30 dark:bg-emerald-500/10'
}

function renderAssistant(content: string): string {
  const html = marked.parse(content, { breaks: true, gfm: true }) as string
  return DOMPurify.sanitize(html, {
    FORBID_TAGS: ['audio', 'embed', 'iframe', 'img', 'object', 'source', 'svg', 'track', 'video'],
    FORBID_ATTR: ['style']
  })
}
</script>

<style scoped>
.conversation-markdown :deep(pre) {
  overflow-x: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  border-radius: 0.375rem;
  padding: 0.75rem;
  background: rgb(0 0 0 / 0.05);
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace;
  font-size: 0.75rem;
  line-height: 1.25rem;
}

.conversation-markdown :deep(code) {
  overflow-wrap: anywhere;
}
</style>
