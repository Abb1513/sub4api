<template>
  <AppLayout>
    <TablePageLayout class="audit-page">
      <template #filters>
        <div class="card space-y-5 p-4 sm:p-6">
          <p class="text-sm text-amber-700 dark:text-amber-300">{{ t('admin.userRequestAudit.sensitivity') }}</p>

          <div class="flex flex-wrap items-end gap-4">
            <div v-for="field in filterFields" :key="field" class="w-full sm:w-auto sm:min-w-[170px]">
              <label :for="`audit-filter-${field}`" class="input-label">{{ t(`admin.userRequestAudit.filters.${field}`) }}</label>
              <input :id="`audit-filter-${field}`" v-model.trim="filters[field]" class="input" @keyup.enter="search" />
            </div>
            <div class="w-full sm:w-auto sm:min-w-[240px]">
              <label for="audit-filter-requested_model" class="input-label">{{ t('admin.userRequestAudit.filters.requested_model') }}</label>
              <Select id="audit-filter-requested_model" v-model="filters.requested_model" :options="modelOptions" searchable creatable :loading="modelsLoading" :aria-label="t('admin.userRequestAudit.filters.requested_model')" />
            </div>
            <div>
              <label for="audit-filter-protocol" class="input-label">{{ t('admin.userRequestAudit.filters.protocol') }}</label>
              <Select id="audit-filter-protocol" v-model="filters.protocol" :options="protocolOptions" :aria-label="t('admin.userRequestAudit.filters.protocol')" />
            </div>
            <div>
              <label for="audit-filter-status" class="input-label">{{ t('admin.userRequestAudit.filters.status') }}</label>
              <input id="audit-filter-status" v-model.trim="filters.status" class="input" @keyup.enter="search" />
            </div>
            <button class="btn btn-primary" :disabled="loading" @click="search">{{ t('common.search') }}</button>
            <button class="btn btn-secondary" :disabled="loading" @click="resetFilters">{{ t('common.reset') }}</button>
          </div>

          <div class="border-t pt-4">
            <div class="flex flex-wrap items-end gap-4">
              <div>
                <label for="audit-export-format" class="input-label">{{ t('admin.userRequestAudit.exportFormat') }}</label>
                <Select id="audit-export-format" v-model="exportFormat" :options="exportOptions" :aria-label="t('admin.userRequestAudit.exportFormat')" />
              </div>
              <button class="btn btn-secondary" :disabled="exporting || clearing" @click="queueExport">
                {{ exporting ? t('common.loading') : selectedConversationIDs.length ? t('admin.userRequestAudit.exportSelected', { count: selectedConversationIDs.length }) : t('admin.userRequestAudit.exportMatching') }}
              </button>
              <button v-if="selectedConversationIDs.length" class="btn btn-secondary" @click="selectedConversationIDs = []">{{ t('admin.userRequestAudit.clearSelection') }}</button>
              <span class="text-xs text-gray-500">{{ t('admin.userRequestAudit.exportWarning') }}</span>
            </div>
            <div class="mt-4 space-y-2">
              <div class="flex items-center justify-between gap-3">
                <h2 class="text-sm font-semibold">{{ t('admin.userRequestAudit.exportJobs') }}</h2>
                <button class="btn btn-secondary" :disabled="jobsLoading || clearing" @click="refreshJobs">{{ t('admin.userRequestAudit.refreshExports') }}</button>
              </div>
              <p v-if="jobsError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ jobsError }}</p>
              <p v-else-if="!jobsLoading && !exportJobs.length" class="text-xs text-gray-500">{{ t('admin.userRequestAudit.noExports') }}</p>
              <ul class="space-y-2" aria-live="polite">
                <li v-for="job in exportJobs" :key="job.id" class="flex flex-wrap items-center gap-3 rounded border border-gray-200 p-3 text-sm dark:border-dark-700">
                  <span>{{ formatTime(job.created_at) }} · {{ job.format.toUpperCase() }}.gz</span>
                  <span>{{ t(`admin.userRequestAudit.jobStatus.${job.status}`) }}</span>
                  <span>{{ t('admin.userRequestAudit.exportRows', { count: job.rows }) }}</span>
                  <span v-if="job.error" class="text-red-600 dark:text-red-400">{{ job.error }}</span>
                  <template v-if="job.status === 'completed'">
                    <span v-if="job.size_bytes !== undefined">{{ t('admin.userRequestAudit.exportSize') }}: {{ formatBytes(job.size_bytes) }}</span>
                    <span v-if="job.expires_at">{{ t('admin.userRequestAudit.exportExpires') }}: {{ formatTime(job.expires_at) }}</span>
                  </template>
                  <button v-if="job.status === 'completed'" class="btn btn-secondary" :disabled="downloadingJobIDs.includes(job.id) || clearing" @click="downloadExport(job)">{{ downloadingJobIDs.includes(job.id) ? t('common.loading') : t('admin.userRequestAudit.download') }}</button>
                </li>
              </ul>
            </div>
          </div>

          <div class="border-t pt-4">
            <div class="mb-3">
              <h2 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('admin.userRequestAudit.captureGroups') }}</h2>
              <p class="mt-1 text-xs text-gray-500">{{ t('admin.userRequestAudit.captureGroupsNote') }}</p>
            </div>
            <div v-if="groupsLoading" class="text-sm text-gray-500">{{ t('common.loading') }}</div>
            <div v-else-if="groupsLoadFailed" class="text-sm text-red-600 dark:text-red-400">{{ t('admin.userRequestAudit.groupsLoadFailed') }}</div>
            <div v-else-if="groups.length === 0" class="text-sm text-gray-500">{{ t('admin.userRequestAudit.noGroups') }}</div>
            <div v-else class="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
              <label v-for="group in groups" :key="group.id" class="flex items-center gap-2 rounded border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
                <input v-model="selectedGroupIDs" :value="group.id" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-800" />
                <span class="min-w-0 flex-1 truncate">{{ group.name }}</span>
                <span class="text-xs" :class="selectedGroupIDs.includes(group.id) ? 'text-emerald-500' : 'text-gray-500'">{{ selectedGroupIDs.includes(group.id) ? t('admin.userRequestAudit.captureEnabled') : t('admin.userRequestAudit.captureDisabled') }}</span>
              </label>
            </div>
          </div>

          <div class="border-t pt-4">
            <div class="flex flex-wrap items-end gap-4">
              <div>
                <label for="audit-retention" class="input-label">{{ t('admin.userRequestAudit.retention') }}</label>
                <input id="audit-retention" v-model.number="retention" type="number" min="1" max="365" class="input w-32" />
              </div>
              <div>
                <label for="audit-cleanup-interval" class="input-label">{{ t('admin.userRequestAudit.cleanupInterval') }}</label>
                <input id="audit-cleanup-interval" v-model.number="cleanupInterval" type="number" min="1" class="input w-32" />
              </div>
              <button class="btn btn-secondary" :disabled="configSaving || !configLoaded || groupsLoadFailed || groupsLoading" @click="saveConfig">{{ t('common.save') }}</button>
            </div>
            <div class="mt-3 flex flex-wrap gap-4 text-xs text-gray-500">
              <span>{{ t('admin.userRequestAudit.totalBytes') }}: {{ formatBytes(status.total_bytes) }}</span>
              <span>{{ t('admin.userRequestAudit.fileCount') }}: {{ status.file_count }}</span>
              <span>{{ t('admin.userRequestAudit.oldest') }}: {{ formatTime(status.oldest_at) }}</span>
              <span>{{ t('admin.userRequestAudit.latest') }}: {{ formatTime(status.latest_at) }}</span>
              <span>{{ t('admin.userRequestAudit.lastCleanup') }}: {{ formatTime(status.last_cleanup_at) }}</span>
              <span>{{ t('admin.userRequestAudit.diskFree') }}: {{ formatBytes(status.disk_free_bytes) }}</span>
              <span>{{ t('admin.userRequestAudit.diskTotal') }}: {{ formatBytes(status.disk_total_bytes) }}</span>
            </div>
            <p v-if="status.recording_paused" role="alert" class="mt-3 text-sm text-amber-700 dark:text-amber-300">{{ t('admin.userRequestAudit.recordingPaused') }} {{ status.pause_reason }}</p>
            <p class="mt-2 text-xs text-gray-500">{{ t('admin.userRequestAudit.diskThreshold') }}</p>
            <div class="mt-3 flex gap-2">
              <button class="btn btn-secondary" :disabled="statusLoading" @click="refreshStatus">{{ t('admin.userRequestAudit.refreshStats') }}</button>
              <button class="btn btn-secondary" :disabled="cleanupLoading || clearing" @click="runCleanup">{{ t('admin.userRequestAudit.cleanupNow') }}</button>
              <button class="btn btn-danger" :disabled="clearing || exporting || cleanupLoading" @click="clearConfirmVisible = true">{{ clearing ? t('common.loading') : t('admin.userRequestAudit.clearAll') }}</button>
            </div>
          </div>
        </div>
      </template>

      <template #table>
        <div class="mb-4 flex gap-2 border-b">
          <button class="border-b-2 px-3 py-2 text-sm" :class="activeTab === 'records' ? 'border-primary-600 text-primary-600' : 'border-transparent text-gray-500'" @click="activeTab = 'records'">{{ t('admin.userRequestAudit.recordsTab') }}</button>
          <button class="border-b-2 px-3 py-2 text-sm" :class="activeTab === 'groups' ? 'border-primary-600 text-primary-600' : 'border-transparent text-gray-500'" @click="activeTab = 'groups'; fetchGroupStats()">{{ t('admin.userRequestAudit.groupsTab') }}</button>
        </div>

        <div v-if="activeTab === 'records'" class="space-y-3">
          <div v-if="loading" class="py-12 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
          <div v-else-if="items.length === 0" class="py-8 text-center text-sm text-gray-500">{{ t('admin.userRequestAudit.empty') }}</div>
          <template v-else>
            <article v-for="row in items" :key="row.id" class="rounded-lg border border-gray-200 bg-white shadow-sm dark:border-dark-700 dark:bg-dark-900">
            <div class="flex flex-wrap items-start gap-3 p-4">
              <input
                :id="`audit-conversation-${row.id}`"
                type="checkbox"
                class="mt-1 h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-800"
                :checked="selectedConversationIDs.includes(row.id)"
                :aria-label="t('admin.userRequestAudit.selectConversation', { id: row.id })"
                @change="toggleSelection(row.id)"
              />
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
                  <label :for="`audit-conversation-${row.id}`" class="cursor-pointer truncate font-semibold text-gray-900 dark:text-gray-100" :title="conversationIdentity(row)">{{ conversationIdentity(row) }}</label>
                  <span class="text-xs text-gray-500 dark:text-dark-400">{{ row.user_label || (row.user_id ? `#${row.user_id}` : '—') }}</span>
                  <span class="text-xs text-gray-500 dark:text-dark-400">{{ formatTime(row.created_at) }}</span>
                </div>
                <div class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-dark-400">
                  <span>{{ t('admin.userRequestAudit.requestCount') }}: {{ row.request_count ?? 1 }}</span>
                  <span>{{ protocolLabel(row.protocol) }} · {{ row.requested_model || '—' }}</span>
                  <span>{{ t('admin.userRequestAudit.status') }}: <strong class="font-medium text-gray-700 dark:text-gray-200">{{ row.status }}</strong></span>
                </div>
              </div>
              <div class="flex shrink-0 items-center gap-3">
                <button type="button" class="text-sm text-primary-600 hover:text-primary-700 dark:text-primary-300" :aria-expanded="expandedConversationID === row.id" @click="toggleConversation(row.id)">
                  {{ expandedConversationID === row.id ? t('admin.userRequestAudit.closeConversation') : t('admin.userRequestAudit.readConversation') }}
                </button>
                <button type="button" class="text-sm text-gray-500 hover:text-gray-700 dark:text-dark-300 dark:hover:text-white" @click="openDetail(row.id)">{{ t('admin.userRequestAudit.detail') }}</button>
              </div>
            </div>
            <div v-if="expandedConversationID === row.id" class="border-t border-gray-200 p-4 dark:border-dark-700">
              <UserRequestAuditTranscript :conversation="conversation" :loading="conversationLoading" :error="conversationError" :loading-more="conversationMoreLoading" :window-size="conversationWindow" @retry="loadConversation(row.id)" @load-more="loadMoreConversation" @load-previous="loadPreviousConversation" />
            </div>
            </article>
          </template>
        </div>
        <div v-else-if="groupLoading" class="py-12 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
        <div v-else-if="groupStats.length === 0" class="py-8 text-center text-sm text-gray-500">{{ t('admin.userRequestAudit.emptyGroups') }}</div>
        <div v-else class="overflow-x-auto">
          <table class="w-full min-w-max text-left text-sm">
            <thead><tr class="border-b"><th class="px-3 py-2">{{ t('admin.userRequestAudit.group') }}</th><th class="px-3 py-2">{{ t('admin.userRequestAudit.total') }}</th><th class="px-3 py-2">{{ t('admin.userRequestAudit.completed') }}</th><th class="px-3 py-2">{{ t('admin.userRequestAudit.failed') }}</th><th class="px-3 py-2">{{ t('admin.userRequestAudit.inputTokens') }}</th><th class="px-3 py-2">{{ t('admin.userRequestAudit.outputTokens') }}</th><th class="px-3 py-2">{{ t('admin.userRequestAudit.latest') }}</th></tr></thead>
            <tbody><tr v-for="stat in groupStats" :key="`${stat.group_id ?? 'none'}-${stat.group_name ?? ''}`" class="border-b"><td class="px-3 py-2">{{ stat.group_name || stat.group_id || '—' }}</td><td class="px-3 py-2">{{ stat.count }}</td><td class="px-3 py-2">{{ stat.completed }}</td><td class="px-3 py-2">{{ stat.failed }}</td><td class="px-3 py-2">{{ stat.input_total }}</td><td class="px-3 py-2">{{ stat.output_total }}</td><td class="px-3 py-2">{{ formatTime(stat.latest_at) }}</td></tr></tbody>
          </table>
        </div>
      </template>
      <template #pagination><Pagination v-if="activeTab === 'records' && total" :total="total" :page="page" :page-size="pageSize" @update:page="onPageChange" @update:pageSize="onPageSizeChange" /></template>
    </TablePageLayout>

    <BaseDialog :show="detailVisible" :title="t('admin.userRequestAudit.detail')" width="wide" @close="detailVisible = false">
      <div v-if="detailLoading" class="py-12 text-center">{{ t('common.loading') }}</div>
      <div v-else-if="detail" class="space-y-4 py-2">
        <div class="grid grid-cols-1 gap-2 text-sm sm:grid-cols-2"><div v-for="key in detailKeys" :key="key"><span class="text-gray-500">{{ key }}:</span> <span class="break-all font-mono">{{ detail[key] ?? '—' }}</span></div></div>
        <div v-if="detail.request_chatml || detail.response_chatml" class="space-y-3">
          <div v-if="detail.request_chatml"><h4 class="font-medium">{{ t('admin.userRequestAudit.requestChatml') }}</h4><pre class="max-h-64 overflow-auto whitespace-pre-wrap rounded bg-gray-100 p-3 text-xs dark:bg-dark-800">{{ detail.request_chatml }}</pre></div>
          <div v-if="detail.response_chatml"><h4 class="font-medium">{{ t('admin.userRequestAudit.responseChatml') }}</h4><pre class="max-h-64 overflow-auto whitespace-pre-wrap rounded bg-gray-100 p-3 text-xs dark:bg-dark-800">{{ detail.response_chatml }}</pre></div>
        </div>
      </div>
    </BaseDialog>
    <ConfirmDialog :show="clearConfirmVisible" :title="t('admin.userRequestAudit.clearAll')" :message="t('admin.userRequestAudit.clearConfirm')" :confirm-text="t('admin.userRequestAudit.clearAll')" danger @confirm="clearArchives" @cancel="clearConfirmVisible = false" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI, type UserRequestAudit, type UserRequestAuditGroupStat, type UserRequestAuditModelFacet, type UserRequestAuditQuery, type UserRequestAuditExportFormat, type UserRequestAuditStatus } from '@/api/admin'
import type { UserRequestAuditConversation, UserRequestAuditExportJob } from '@/api/admin/userRequestAudit'
import type { AdminGroup } from '@/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import UserRequestAuditTranscript from '@/components/admin/UserRequestAuditTranscript.vue'
import { useAppStore } from '@/stores'

const { t } = useI18n()
const appStore = useAppStore()
const loading = ref(false)
const items = ref<UserRequestAudit[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const retention = ref(7)
const cleanupInterval = ref(24)
const selectedGroupIDs = ref<number[]>([])
const groups = ref<AdminGroup[]>([])
const groupsLoading = ref(false)
const groupsLoadFailed = ref(false)
const configLoaded = ref(false)
const configSaving = ref(false)
const detailVisible = ref(false)
const detailLoading = ref(false)
const detail = ref<UserRequestAudit | null>(null)
const expandedConversationID = ref<number | null>(null)
const conversation = ref<UserRequestAuditConversation | null>(null)
const conversationLoading = ref(false)
const conversationError = ref('')
const conversationMoreLoading = ref(false)
// A window of 20 requests keeps a first read small while still showing real
// progress in long sessions; the server clamps oversized requests.
const conversationWindow = 20
let conversationRequest = 0
let conversationController: AbortController | undefined
const exporting = ref(false)
const exportFormat = ref<UserRequestAuditExportFormat>('jsonl')
const selectedConversationIDs = ref<number[]>([])
const exportJobs = ref<UserRequestAuditExportJob[]>([])
const jobsLoading = ref(false)
const jobsError = ref('')
const downloadingJobIDs = ref<string[]>([])
const clearConfirmVisible = ref(false)
const clearing = ref(false)
let jobsTimer: ReturnType<typeof setTimeout> | undefined
let jobsController: AbortController | undefined
let unmounted = false
const activeTab = ref<'records' | 'groups'>('records')
const groupLoading = ref(false)
const groupStats = ref<UserRequestAuditGroupStat[]>([])
const status = ref<UserRequestAuditStatus>({ total_bytes: 0, file_count: 0, disk_total_bytes: 0, disk_free_bytes: 0, recording_paused: false })
const statusLoading = ref(false)
const cleanupLoading = ref(false)
let detailRequest = 0

const filters = reactive<Record<string, string>>({ user_id: '', group_id: '', group_name: '', q: '', requested_model: '', response_id: '', client_request_id: '', status: '', protocol: '', start_time: '', end_time: '' })
const filterFields = ['q', 'user_id', 'group_id', 'group_name', 'response_id', 'client_request_id', 'start_time', 'end_time']
const modelFacets = ref<UserRequestAuditModelFacet[]>([])
const modelsLoading = ref(false)
let modelsController: AbortController | undefined
const protocolOptions = [{ value: '', label: '—' }, { value: 'anthropic_messages', label: 'Anthropic' }, { value: 'openai_responses', label: 'OpenAI Responses' }, { value: 'openai_chat_completions', label: 'OpenAI Chat' }, { value: 'openai_responses_ws', label: 'Responses WS' }, { value: 'typesafe_systemone', label: 'TypeSafe System One' }]
const exportOptions = [{ value: 'jsonl', label: 'JSONL' }, { value: 'json', label: 'JSON' }]
const modelOptions = computed<SelectOption[]>(() => {
  const options: SelectOption[] = [{ value: '', label: t('admin.userRequestAudit.allModels') }]
  for (const facet of modelFacets.value) {
    options.push({ value: facet.requested_model, label: `${facet.requested_model} · ${protocolLabel(facet.protocol)} · ${t('admin.userRequestAudit.modelRequests', { count: facet.requests })}` })
  }
  return options
})
const detailKeys = ['id', 'created_at', 'updated_at', 'expires_at', 'user_id', 'api_key_id', 'group_id', 'group_name', 'protocol', 'endpoint', 'requested_model', 'upstream_model', 'client_request_id', 'response_id', 'previous_response_id', 'fallback_hash', 'status', 'input_usage', 'output_usage', 'cache_usage', 'conversation_key', 'last_error'] as const

function protocolLabel(protocol: string) {
  return protocolOptions.find(option => option.value === protocol)?.label || protocol
}

function formatTime(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() <= 1) return '—'
  return date.toLocaleString()
}

function formatBytes(value: number) {
  if (!Number.isFinite(value)) return '—'
  if (value < 1024) return `${value} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)) - 1, units.length - 1)
  return `${(value / 1024 ** (index + 1)).toFixed(2)} ${units[index]}`
}

function conversationIdentity(row: UserRequestAudit): string {
  const key = row.conversation_key?.trim()
  if (key) return key.length > 96 ? `${key.slice(0, 93)}…` : key
  return `${protocolLabel(row.protocol)} · ${row.endpoint} · ${row.requested_model || t('admin.userRequestAudit.conversation')}`
}

function toggleSelection(id: number): void {
  selectedConversationIDs.value = selectedConversationIDs.value.includes(id)
    ? selectedConversationIDs.value.filter(selectedID => selectedID !== id)
    : [...selectedConversationIDs.value, id]
}

function closeConversation(): void {
  conversationRequest++
  conversationController?.abort()
  conversationController = undefined
  expandedConversationID.value = null
  conversation.value = null
  conversationLoading.value = false
  conversationMoreLoading.value = false
  conversationError.value = ''
}

function toggleConversation(id: number): void {
  if (expandedConversationID.value === id) {
    closeConversation()
    return
  }
  void loadConversation(id)
}

async function loadConversation(id: number): Promise<void> {
  const token = ++conversationRequest
  conversationController?.abort()
  const controller = new AbortController()
  conversationController = controller
  expandedConversationID.value = id
  conversation.value = null
  conversationError.value = ''
  conversationMoreLoading.value = false
  conversationLoading.value = true
  try {
    const value = await adminAPI.userRequestAudit.getConversationWindow(id, 0, conversationWindow, controller.signal)
    if (token === conversationRequest && !controller.signal.aborted && !unmounted) conversation.value = value
  } catch (e: any) {
    if (token === conversationRequest && !controller.signal.aborted && !unmounted) conversationError.value = e?.message || t('admin.userRequestAudit.conversationLoadFailed')
  } finally {
    if (token === conversationRequest && !controller.signal.aborted && !unmounted) conversationLoading.value = false
  }
}

// Replace the visible window instead of accumulating all earlier messages.
// Previous windows remain available from the archive, not retained in the DOM.
function loadMoreConversation(): Promise<void> {
  return loadConversationPage(false)
}

function loadPreviousConversation(): Promise<void> {
  return loadConversationPage(true)
}

async function loadConversationPage(previous: boolean): Promise<void> {
  const base = conversation.value
  const id = expandedConversationID.value
  if (!base || !id || conversationMoreLoading.value) return
  if (previous ? base.request_start <= 1 : base.request_end >= base.request_count) return
  const offset = previous ? Math.max(0, base.request_start - 1 - conversationWindow) : base.request_end
  const token = ++conversationRequest
  const controller = new AbortController()
  conversationController = controller
  conversationMoreLoading.value = true
  try {
    const next = await adminAPI.userRequestAudit.getConversationWindow(id, offset, conversationWindow, controller.signal)
    if (token !== conversationRequest || controller.signal.aborted || unmounted) return
    if (next.request_start !== offset + 1) {
      // The conversation changed underneath the reader; restart cleanly.
      await loadConversation(id)
      return
    }
    conversation.value = next
  } catch (e: any) {
    if (token === conversationRequest && !controller.signal.aborted && !unmounted) appStore.showError(e?.message || t('admin.userRequestAudit.conversationLoadFailed'))
  } finally {
    if (token === conversationRequest && !controller.signal.aborted && !unmounted) conversationMoreLoading.value = false
  }
}

function query(): UserRequestAuditQuery {
  const result: UserRequestAuditQuery = { page: page.value, page_size: pageSize.value }
  for (const key of ['user_id', 'group_id'] as const) {
    const value = filters[key]?.trim()
    if (!value) continue
    const id = Number(value)
    if (!/^\d+$/.test(value) || !Number.isSafeInteger(id) || id <= 0) {
      throw new Error(t('admin.userRequestAudit.invalidID', { field: t(`admin.userRequestAudit.filters.${key}`) }))
    }
    result[key] = id
  }
  for (const key of ['group_name', 'q', 'requested_model', 'response_id', 'client_request_id', 'status', 'protocol'] as const) {
    const value = filters[key]?.trim()
    if (value) result[key] = value
  }
  for (const key of ['start_time', 'end_time'] as const) {
    const value = filters[key]?.trim()
    if (!value) continue
    const date = new Date(value)
    if (!Number.isFinite(date.getTime())) {
      throw new Error(t('admin.userRequestAudit.invalidTime', { field: t(`admin.userRequestAudit.filters.${key}`) }))
    }
    result[key] = date.toISOString()
  }
  return result
}

async function fetchItems() {
  loading.value = true
  try {
    const res = await adminAPI.userRequestAudit.list(query())
    items.value = res.items
    total.value = res.total
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function fetchGroupStats() {
  groupLoading.value = true
  try {
    groupStats.value = await adminAPI.userRequestAudit.getGroupStats(query())
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.loadFailed'))
  } finally {
    groupLoading.value = false
  }
}

async function fetchModelFacets() {
  modelsController?.abort()
  const controller = new AbortController()
  modelsController = controller
  modelsLoading.value = true
  try {
    // Facets follow the protocol/group/time filters only; the model itself is
    // the value being chosen, so the picker keeps offering every other model.
    const params = query()
    delete params.requested_model
    const facets = await adminAPI.userRequestAudit.getModels(params)
    if (controller.signal.aborted || unmounted) return
    modelFacets.value = facets
  } catch (e: any) {
    if (controller.signal.aborted || unmounted) return
    appStore.showError(e?.message || t('admin.userRequestAudit.loadFailed'))
  } finally {
    if (!controller.signal.aborted && !unmounted) modelsLoading.value = false
  }
}

function search() {
  selectedConversationIDs.value = []
  closeConversation()
  page.value = 1
  fetchItems()
  fetchModelFacets()
  if (activeTab.value === 'groups') fetchGroupStats()
}

function resetFilters() {
  Object.keys(filters).forEach(key => { filters[key] = '' })
  search()
}

function onPageChange(value: number) {
  closeConversation()
  page.value = value
  fetchItems()
}

function onPageSizeChange(value: number) {
  closeConversation()
  pageSize.value = value
  page.value = 1
  fetchItems()
}

async function queueExport() {
  exporting.value = true
  try {
    const job = await adminAPI.userRequestAudit.export(query(), exportFormat.value, selectedConversationIDs.value)
    exportJobs.value = [job, ...exportJobs.value.filter(existing => existing.id !== job.id)]
    appStore.showSuccess(t('admin.userRequestAudit.exportQueued'))
    await refreshJobs()
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.exportFailed'))
  } finally {
    exporting.value = false
  }
}

async function refreshJobs() {
  if (unmounted || clearing.value) return
  clearTimeout(jobsTimer)
  jobsController?.abort()
  const controller = new AbortController()
  jobsController = controller
  jobsLoading.value = true
  try {
    const jobs = await adminAPI.userRequestAudit.getExports(controller.signal)
    if (controller.signal.aborted || unmounted) return
    exportJobs.value = jobs
    jobsError.value = ''
  } catch (e: any) {
    if (controller.signal.aborted || unmounted) return
    jobsError.value = e?.message || t('admin.userRequestAudit.loadFailed')
  } finally {
    if (!controller.signal.aborted && !unmounted) {
      jobsLoading.value = false
      if (exportJobs.value.some(job => job.status === 'queued' || job.status === 'running')) {
        jobsTimer = setTimeout(refreshJobs, 3000)
      }
    }
  }
}

async function downloadExport(job: UserRequestAuditExportJob) {
  downloadingJobIDs.value.push(job.id)
  try {
    const result = await adminAPI.userRequestAudit.downloadExport(job)
    if (unmounted) return
    const url = URL.createObjectURL(result.blob)
    const link = document.createElement('a')
    link.href = url
    link.download = result.filename
    document.body.appendChild(link)
    link.click()
    link.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.exportFailed'))
  } finally {
    downloadingJobIDs.value = downloadingJobIDs.value.filter(id => id !== job.id)
  }
}

async function clearArchives() {
  if (clearing.value) return
  clearConfirmVisible.value = false
  clearing.value = true
  clearTimeout(jobsTimer)
  jobsController?.abort()
  try {
    const result = await adminAPI.userRequestAudit.clear()
    selectedConversationIDs.value = []
    closeConversation()
    detailRequest++
    detailVisible.value = false
    detail.value = null
    page.value = 1
    appStore.showSuccess(result?.message || t('admin.userRequestAudit.clearSuccess'))
    await Promise.all([fetchItems(), refreshStatus(), fetchGroupStats(), fetchModelFacets()])
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.clearFailed'))
  } finally {
    clearing.value = false
    await refreshJobs()
  }
}

async function openDetail(id: number) {
  const token = ++detailRequest
  detailVisible.value = true
  detailLoading.value = true
  detail.value = null
  try {
    const value = await adminAPI.userRequestAudit.get(id)
    if (token === detailRequest) detail.value = value
  } catch (e: any) {
    if (token === detailRequest) {
      detailVisible.value = false
      appStore.showError(e?.message || t('admin.userRequestAudit.loadFailed'))
    }
  } finally {
    if (token === detailRequest) detailLoading.value = false
  }
}

async function saveConfig() {
  if (retention.value < 1 || retention.value > 365 || cleanupInterval.value < 1) {
    appStore.showError(t('admin.userRequestAudit.retentionInvalid'))
    return
  }
  configSaving.value = true
  try {
    const config = await adminAPI.userRequestAudit.updateConfig({ retention_days: retention.value, cleanup_interval_hours: cleanupInterval.value, group_ids: selectedGroupIDs.value })
    retention.value = config.retention_days
    cleanupInterval.value = config.cleanup_interval_hours
    selectedGroupIDs.value = Array.isArray(config.group_ids) ? config.group_ids : []
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.configFailed'))
  } finally {
    configSaving.value = false
  }
}

async function refreshStatus() {
  statusLoading.value = true
  try {
    status.value = await adminAPI.userRequestAudit.getStatus()
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.loadFailed'))
  } finally {
    statusLoading.value = false
  }
}

async function runCleanup() {
  cleanupLoading.value = true
  try {
    await adminAPI.userRequestAudit.cleanup()
    await refreshStatus()
  } catch (e: any) {
    appStore.showError(e?.message || t('admin.userRequestAudit.cleanupFailed'))
  } finally {
    cleanupLoading.value = false
  }
}

async function loadConfig() {
  try {
    const config = await adminAPI.userRequestAudit.getConfig()
    if (!Array.isArray(config.group_ids) || config.group_ids.some(id => !Number.isInteger(id) || id <= 0)) throw new Error(t('admin.userRequestAudit.configInvalid'))
    retention.value = config.retention_days
    cleanupInterval.value = config.cleanup_interval_hours
    selectedGroupIDs.value = [...config.group_ids]
    configLoaded.value = true
  } catch (e: any) {
    configLoaded.value = false
    appStore.showError(e?.message || t('admin.userRequestAudit.configFailed'))
  }
}

async function loadGroups() {
  groupsLoading.value = true
  try {
    groups.value = await adminAPI.groups.getAllIncludingInactive()
    groupsLoadFailed.value = false
  } catch (e: any) {
    groupsLoadFailed.value = true
    appStore.showError(e?.message || t('admin.userRequestAudit.groupsLoadFailed'))
  } finally {
    groupsLoading.value = false
  }
}

onMounted(() => {
  fetchItems()
  fetchModelFacets()
  refreshStatus()
  loadConfig()
  loadGroups()
  refreshJobs()
})

onUnmounted(() => {
  unmounted = true
  detailRequest++
  clearTimeout(jobsTimer)
  jobsController?.abort()
  conversationRequest++
  conversationController?.abort()
  modelsController?.abort()
})
</script>

<style scoped>
.audit-page {
  height: auto !important;
}

.audit-page :deep(.layout-section-scrollable) {
  flex: none;
  min-height: auto;
}

.audit-page :deep(.table-scroll-container) {
  height: auto;
  min-height: 0;
  overflow: visible;
}

.audit-page :deep(.table-wrapper) {
  flex: none;
  min-height: 0;
  overflow-x: auto;
  overflow-y: visible;
}
</style>
