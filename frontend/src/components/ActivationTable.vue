<script setup lang="ts">
import { computed } from "vue";
import { ElMessage, TableV2FixedDir } from "element-plus";
import type { Column } from "element-plus";

import { activationExpiryPresentation } from "../activationExpiry";
import {
  accountLoginStatus,
  activationStatus,
  formatNumber,
  formatTime,
} from "../normalizers";
import type { ActivationView, GoPayLoginStatusView } from "../types";

const ROW_HEIGHT = 92;
const HEADER_HEIGHT = 48;
const MAX_VISIBLE_ROWS = 7;
const ERROR_COLUMN_MIN_WIDTH = 220;
const TABLE_VERTICAL_SCROLLBAR_SIZE = 6;

const props = defineProps<{
  activations: ActivationView[];
  busyById: Record<string, boolean>;
  getLoginStatus: (
    activation: ActivationView,
  ) => GoPayLoginStatusView | undefined;
  nowMs: number;
}>();

const emit = defineEmits<{
  success: [activation: ActivationView];
  delete: [activation: ActivationView];
}>();

const columns: Column[] = [
  {
    key: "phone",
    dataKey: "phone",
    title: "手机号码 / ID",
    width: 218,
  },
  { key: "status", dataKey: "status", title: "激活状态", width: 142 },
  {
    key: "loginStatus",
    dataKey: "loginStatus",
    title: "GoPay 登录",
    width: 182,
  },
  { key: "balance", dataKey: "balance", title: "余额", width: 112 },
  { key: "provider", dataKey: "provider", title: "供应商", width: 124 },
  { key: "expiresAt", dataKey: "expiresAt", title: "到期时间", width: 166 },
  { key: "loginCode", dataKey: "loginCode", title: "登录验证码", width: 136 },
  { key: "pinCode", dataKey: "pinCode", title: "改 PIN 验证码", width: 146 },
  {
    key: "subsequentCodes",
    dataKey: "subsequentCodes",
    title: "后续验证码",
    width: 238,
  },
  {
    key: "error",
    dataKey: "error",
    title: "错误信息",
    width: ERROR_COLUMN_MIN_WIDTH,
  },
  {
    key: "actions",
    dataKey: "actions",
    title: "操作",
    width: 132,
    align: "center",
  },
];

const compactColumns: Column[] = columns.map((column) => ({
  ...column,
  fixed: undefined,
}));

const nonErrorColumnsWidth = columns.reduce(
  (width, column) =>
    column.dataKey === "error" ? width : width + column.width,
  0,
);

const tableHeight = computed(
  () =>
    HEADER_HEIGHT +
    Math.min(props.activations.length, MAX_VISIBLE_ROWS) * ROW_HEIGHT +
    12,
);

function columnsForWidth(width: number): Column[] {
  const baseColumns = width < 720 ? compactColumns : columns;
  const errorColumnWidth = Math.max(
    ERROR_COLUMN_MIN_WIDTH,
    width - TABLE_VERTICAL_SCROLLBAR_SIZE - nonErrorColumnsWidth,
  );

  if (errorColumnWidth === ERROR_COLUMN_MIN_WIDTH) return baseColumns;

  return baseColumns.map((column) =>
    column.dataKey === "error"
      ? { ...column, width: errorColumnWidth }
      : column,
  );
}

function statusMeta(activation: ActivationView) {
  return activationStatus(activation.status);
}

function loginStatus(
  activation: ActivationView,
): GoPayLoginStatusView | undefined {
  return props.getLoginStatus(activation);
}

function loginMeta(activation: ActivationView) {
  const status = loginStatus(activation);
  return status ? accountLoginStatus(status.status) : undefined;
}

function expiry(activation: ActivationView) {
  return activationExpiryPresentation(
    activation.status,
    activation.expiresAt,
    activation.finishedAt,
    props.nowMs,
  );
}

function canOperate(activation: ActivationView): boolean {
  return (
    !activation.finishedAt &&
    ["polling", "active", "awaiting_subsequent_code"].includes(
      activation.status,
    )
  );
}

function isPolling(activation: ActivationView): boolean {
  return (
    !activation.finishedAt &&
    ["polling", "active", "awaiting_subsequent_code", "pin_changed"].includes(
      activation.status,
    )
  );
}

function rowClass({
  rowData,
  rowIndex,
}: {
  rowData: ActivationView;
  rowIndex: number;
}): string {
  return [
    "activation-table-row",
    rowIndex % 2 ? "activation-table-row--striped" : "",
    isPolling(rowData) ? "activation-table-row--polling" : "",
  ]
    .filter(Boolean)
    .join(" ");
}

function subsequentCodesTitle(activation: ActivationView): string {
  return activation.subsequentCodes
    .map((item, index) => {
      const receivedAt = item.receivedAt
        ? ` · ${formatTime(item.receivedAt)}`
        : "";
      return `${index + 1}. ${item.code}${receivedAt}`;
    })
    .join("\n");
}

async function copyPhone(activation: ActivationView): Promise<void> {
  if (!activation.phone || activation.phone === "—") return;
  try {
    await navigator.clipboard.writeText(activation.phone);
    ElMessage.success("号码已复制");
  } catch {
    ElMessage.warning("复制失败，请手动选择号码");
  }
}
</script>

<template>
  <div class="activation-table-shell" :style="{ height: `${tableHeight}px` }">
    <el-auto-resizer>
      <template #default="{ height, width }">
        <el-table-v2
          class="activation-virtual-table"
          :columns="columnsForWidth(width)"
          :data="activations"
          :width="width"
          :height="height"
          :header-height="HEADER_HEIGHT"
          :row-height="ROW_HEIGHT"
          :row-class="rowClass"
          :v-scrollbar-size="TABLE_VERTICAL_SCROLLBAR_SIZE"
          :cache="4"
          row-key="id"
          fixed
          scrollbar-always-on
        >
          <template #cell="{ column, rowData }">
            <div
              v-if="column.dataKey === 'phone'"
              class="activation-table__identity"
            >
              <button
                class="activation-table__phone"
                type="button"
                title="复制号码"
                @click="copyPhone(rowData)"
              >
                <span>{{ rowData.phone }}</span>
                <span class="activation-table__copy" aria-hidden="true">⧉</span>
              </button>
              <span
                class="activation-table__reference"
                :title="rowData.activationId || rowData.id"
              >
                ID&nbsp; {{ rowData.activationId || rowData.id }}
              </span>
            </div>

            <div
              v-else-if="column.dataKey === 'status'"
              class="activation-table__stack"
            >
              <el-tag
                :type="statusMeta(rowData).type"
                effect="light"
                round
                size="small"
              >
                <span
                  v-if="statusMeta(rowData).active"
                  class="status-pulse"
                  aria-hidden="true"
                />
                {{ statusMeta(rowData).label }}
              </el-tag>
              <small v-if="isPolling(rowData)" class="activation-table__polling"
                >每 2 秒轮询</small
              >
            </div>

            <div
              v-else-if="column.dataKey === 'loginStatus'"
              class="activation-table__login"
              :class="
                loginStatus(rowData) ? `is-${loginStatus(rowData)?.status}` : ''
              "
              :title="loginStatus(rowData)?.message"
            >
              <template v-if="loginStatus(rowData) && loginMeta(rowData)">
                <strong>
                  <span
                    class="activation-table__login-dot"
                    aria-hidden="true"
                  />
                  GoPay · {{ loginMeta(rowData)?.label }}
                </strong>
                <small v-if="loginStatus(rowData)?.checkedAt">
                  检查于 {{ formatTime(loginStatus(rowData)?.checkedAt) }}
                </small>
                <small v-if="loginStatus(rowData)?.refreshed"
                  >已自动刷新登录凭据</small
                >
              </template>
              <span v-else class="activation-table__muted">—</span>
            </div>

            <div
              v-else-if="column.dataKey === 'balance'"
              class="activation-table__stack"
            >
              <strong
                :class="{
                  'activation-table__balance-positive':
                    (rowData.balance ?? 0) >= 1,
                }"
              >
                {{
                  rowData.balance === undefined
                    ? "查询中"
                    : `${formatNumber(rowData.balance)} RP`
                }}
              </strong>
            </div>

            <span
              v-else-if="column.dataKey === 'provider'"
              class="activation-table__ellipsis"
              :title="rowData.provider || '—'"
            >
              {{ rowData.provider || "—" }}
            </span>

            <div
              v-else-if="column.dataKey === 'expiresAt'"
              class="activation-table__stack"
            >
              <strong>{{ expiry(rowData).label }}</strong>
              <small
                v-if="expiry(rowData).countdown"
                class="activation-table__countdown"
              >
                倒计时 {{ expiry(rowData).countdown }}
              </small>
            </div>

            <code
              v-else-if="column.dataKey === 'loginCode'"
              class="activation-table__code"
              :class="{ 'is-waiting': !rowData.loginCode }"
            >
              {{ rowData.loginCode || "等待接收" }}
            </code>

            <code
              v-else-if="column.dataKey === 'pinCode'"
              class="activation-table__code"
              :class="{ 'is-waiting': !rowData.pinCode }"
            >
              {{ rowData.pinCode || "等待接收" }}
            </code>

            <div
              v-else-if="column.dataKey === 'subsequentCodes'"
              class="activation-table__subsequent"
              :title="subsequentCodesTitle(rowData)"
            >
              <template v-if="rowData.subsequentCodes.length">
                <code
                  v-for="(item, index) in rowData.subsequentCodes"
                  :key="item.id"
                >
                  {{ index + 1 }}. {{ item.code }}
                </code>
              </template>
              <span v-else class="activation-table__muted">
                {{ isPolling(rowData) ? "等待新的验证码…" : "暂无验证码" }}
              </span>
            </div>

            <span
              v-else-if="column.dataKey === 'error'"
              class="activation-table__error"
              :class="{ 'is-empty': !rowData.error }"
              :title="rowData.error"
            >
              <span v-if="rowData.error" aria-hidden="true">!</span>
              {{ rowData.error || "—" }}
            </span>

            <div
              v-else-if="column.dataKey === 'actions'"
              class="activation-table__actions"
            >
              <template v-if="canOperate(rowData)">
                <el-button
                  type="success"
                  size="small"
                  :loading="busyById[rowData.id]"
                  @click.stop="emit('success', rowData)"
                >
                  成功
                </el-button>
                <el-button
                  type="danger"
                  plain
                  size="small"
                  :loading="busyById[rowData.id]"
                  @click.stop="emit('delete', rowData)"
                >
                  删除
                </el-button>
              </template>
              <span v-else class="activation-table__muted">—</span>
            </div>
          </template>
        </el-table-v2>
      </template>
    </el-auto-resizer>
  </div>
</template>

<style scoped>
.activation-table-shell {
  width: 100%;
  min-height: 152px;
  overflow: hidden;
  border: 1px solid var(--line);
  border-radius: 10px;
  background: var(--surface);
  box-shadow: 0 8px 28px rgba(34, 50, 80, 0.045);
}

.activation-virtual-table {
  --el-table-border-color: #e5eaf1;
  --el-table-header-bg-color: #f6f8fb;
  --el-table-header-text-color: #536177;
  --el-table-row-hover-bg-color: #edf4ff;
  --el-table-bg-color: #fff;
  color: var(--ink);
  font-size: 12px;
}

.activation-virtual-table :deep(.el-table-v2__header-row) {
  border-bottom-color: #dce3ed;
}

.activation-virtual-table :deep(.el-table-v2__header-cell) {
  padding: 0 12px;
  border-right: 1px solid #e7ebf1;
  font-size: 11px;
  font-weight: 750;
  letter-spacing: 0.02em;
}

.activation-virtual-table :deep(.el-table-v2__row-cell) {
  padding: 0 12px;
  border-right: 1px solid #edf0f5;
}

.activation-virtual-table :deep(.activation-table-row--striped) {
  background: #fafbfd;
}

.activation-virtual-table :deep(.activation-table-row--polling) {
  box-shadow: inset 3px 0 #31b989;
}

.activation-virtual-table :deep(.el-table-v2__left),
.activation-virtual-table :deep(.el-table-v2__right) {
  box-shadow: 3px 0 10px rgba(35, 49, 75, 0.08);
}

.activation-virtual-table :deep(.el-table-v2__right) {
  box-shadow: -3px 0 10px rgba(35, 49, 75, 0.08);
}

.activation-table__identity,
.activation-table__stack,
.activation-table__login {
  display: flex;
  min-width: 0;
  flex-direction: column;
  justify-content: center;
  gap: 5px;
}

.activation-table__phone {
  display: flex;
  align-items: center;
  gap: 7px;
  overflow: hidden;
  max-width: 100%;
  padding: 0;
  color: #142139;
  border: 0;
  background: transparent;
  cursor: pointer;
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", monospace;
  font-size: 15px;
  font-weight: 750;
  line-height: 1.35;
  text-align: left;
  white-space: nowrap;
}

.activation-table__phone > span:first-child {
  overflow: hidden;
  text-overflow: ellipsis;
}

.activation-table__phone:hover,
.activation-table__phone:focus-visible {
  color: var(--accent);
}

.activation-table__phone:focus-visible {
  border-radius: 3px;
  outline: 2px solid rgba(34, 104, 242, 0.28);
  outline-offset: 2px;
}

.activation-table__copy {
  flex: none;
  color: #95a3b7;
  font-size: 13px;
  font-weight: 400;
}

.activation-table__reference,
.activation-table__stack small,
.activation-table__login small {
  overflow: hidden;
  color: #8a97aa;
  font-size: 10px;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.activation-table__stack strong {
  overflow: hidden;
  font-size: 12px;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.activation-table__polling,
.activation-table__stack .activation-table__countdown {
  color: #2374df;
  font-family: "SFMono-Regular", Consolas, monospace;
  font-weight: 700;
}

.activation-table__login {
  color: #647187;
}

.activation-table__login strong {
  display: flex;
  align-items: center;
  gap: 6px;
  overflow: hidden;
  font-size: 11px;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.activation-table__login.is-valid {
  color: #177b5d;
}

.activation-table__login.is-invalid {
  color: #b13a47;
}

.activation-table__login.is-checking {
  color: #245fc8;
}

.activation-table__login-dot {
  width: 6px;
  height: 6px;
  flex: none;
  border-radius: 50%;
  background: currentColor;
  box-shadow: 0 0 0 3px rgba(82, 97, 120, 0.1);
}

.activation-table__login.is-checking .activation-table__login-dot {
  animation: pulse 1.8s infinite;
}

.activation-table__balance-positive {
  color: var(--green);
}

.activation-table__ellipsis,
.activation-table__error {
  display: block;
  overflow: hidden;
  width: 100%;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.activation-table__code,
.activation-table__subsequent code {
  color: #205fd8;
  font-family: "SFMono-Regular", Consolas, monospace;
  font-size: 13px;
  font-weight: 750;
  letter-spacing: 0.04em;
}

.activation-table__code.is-waiting,
.activation-table__muted {
  color: #98a4b5;
  font-family: inherit;
  font-size: 11px;
  font-weight: 400;
  letter-spacing: 0;
}

.activation-table__subsequent {
  display: flex;
  align-items: center;
  gap: 7px;
  overflow: hidden;
  width: 100%;
  white-space: nowrap;
}

.activation-table__subsequent code {
  flex: none;
  padding-right: 7px;
  border-right: 1px solid #dfe5ed;
  font-size: 12px;
}

.activation-table__error {
  color: #c7434e;
  font-size: 11px;
}

.activation-table__error > span {
  display: inline-grid;
  width: 15px;
  height: 15px;
  margin-right: 4px;
  color: #fff;
  border-radius: 50%;
  background: #e85a64;
  font-size: 10px;
  font-weight: 800;
  place-items: center;
}

.activation-table__error.is-empty {
  color: #a2adbc;
}

.activation-table__actions {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  width: 100%;
}

.activation-table__actions :deep(.el-button) {
  min-width: 52px;
  margin-left: 0;
  padding: 7px 9px;
  border-radius: 6px;
}

@media (max-width: 719px) {
  .activation-table-shell {
    border-radius: 8px;
  }

  .activation-virtual-table :deep(.el-table-v2__header-cell),
  .activation-virtual-table :deep(.el-table-v2__row-cell) {
    padding-right: 10px;
    padding-left: 10px;
  }
}
</style>
