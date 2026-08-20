<script setup lang="ts">
import { keysApi } from "@/api/keys";
import type { BalanceHistory } from "@/types/models";
import { CloseOutline } from "@vicons/ionicons5";
import {
  NButton,
  NCard,
  NDataTable,
  NEmpty,
  NIcon,
  NModal,
  NPagination,
  NSpin,
  NTag,
} from "naive-ui";
import { computed, h, ref, watch } from "vue";
import { useI18n } from "vue-i18n";

interface Props {
  show: boolean;
  groupId: number | null;
  groupName: string;
  keyId: number | null;
  keyDisplay: string;
}

interface Emits {
  (e: "update:show", value: boolean): void;
}

const props = defineProps<Props>();
const emit = defineEmits<Emits>();

const { t } = useI18n();
const loading = ref(false);
const histories = ref<BalanceHistory[]>([]);
const currentPage = ref(1);
const pageSize = ref(10);
const total = ref(0);

const modalVisible = computed({
  get: () => props.show,
  set: (value: boolean) => emit("update:show", value),
});

const columns = computed(() => [
  {
    title: t("keys.balanceHistoryQueriedAt"),
    key: "queried_at",
    render: (row: BalanceHistory) => formatDateTime(row.queried_at),
  },
  {
    title: t("keys.totalBalance"),
    key: "balance_total",
    render: (row: BalanceHistory) =>
      row.balance_total
        ? `${row.balance_total}${row.currency ? " " + row.currency : ""}`
        : "N/A",
  },
  {
    title: t("keys.totalUsed"),
    key: "balance_used",
    render: (row: BalanceHistory) =>
      row.balance_used && row.balance_used !== "N/A" ? row.balance_used : "N/A",
  },
  {
    title: t("common.status"),
    key: "status",
    render: (row: BalanceHistory) =>
      row.status
        ? h(
            NTag,
            { size: "small", type: "success", bordered: false },
            () => row.status
          )
        : "-",
  },
]);

const hasData = computed(() => histories.value.length > 0);

watch(
  () => props.show,
  (visible) => {
    if (visible && props.groupId && props.keyId) {
      currentPage.value = 1;
      loadHistory();
    }
  }
);

watch(currentPage, () => {
  if (props.show) {
    loadHistory();
  }
});

async function loadHistory() {
  if (!props.groupId || !props.keyId) {
    return;
  }

  loading.value = true;
  try {
    const res = await keysApi.getBalanceHistory(props.groupId, {
      page: currentPage.value,
      page_size: pageSize.value,
      key_id: props.keyId,
    });
    histories.value = res.items || [];
    total.value = res.pagination?.total_items || 0;
  } catch {
    histories.value = [];
    total.value = 0;
  } finally {
    loading.value = false;
  }
}

function handlePageChange(page: number) {
  currentPage.value = page;
}

function handleClose() {
  modalVisible.value = false;
}

function formatDateTime(dt: string): string {
  if (!dt) return "-";
  const d = new Date(dt);
  if (isNaN(d.getTime())) return dt;
  return d.toLocaleString();
}
</script>

<template>
  <n-modal :show="modalVisible" @update:show="handleClose" class="balance-history-modal">
    <n-card
      class="balance-history-card"
      :title="t('keys.balanceHistoryTitle', { key: keyDisplay })"
      :bordered="false"
      size="huge"
      role="dialog"
      aria-modal="true"
    >
      <template #header-extra>
        <n-button quaternary circle @click="handleClose">
          <template #icon>
            <n-icon :component="CloseOutline" />
          </template>
        </n-button>
      </template>

      <div class="modal-body">
        <n-spin :show="loading">
          <n-data-table
            v-if="hasData"
            :columns="columns"
            :data="histories"
            :bordered="false"
            size="small"
            :pagination="false"
            :max-height="400"
          />
          <n-empty v-else :description="t('keys.balanceHistoryEmpty')" />
        </n-spin>
      </div>

      <template v-if="hasData" #footer>
        <div class="modal-footer">
          <n-pagination
            :page="currentPage"
            :page-size="pageSize"
            :item-count="total"
            :page-slot="5"
            size="medium"
            @update:page="handlePageChange"
          />
          <n-button size="small" @click="handleClose">{{ t("common.close") }}</n-button>
        </div>
      </template>
    </n-card>
  </n-modal>
</template>

<style scoped>
.balance-history-modal {
  width: 700px;
  max-width: 90vw;
  --n-color: var(--modal-color);
}

.modal-body {
  min-height: 200px;
}

.modal-footer {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
}

:deep(.n-card-header) {
  border-bottom: 1px solid var(--border-color);
  padding: 10px 20px;
}

:deep(.n-card__content) {
  padding: 16px 20px;
}

:deep(.n-card__footer) {
  border-top: 1px solid var(--border-color);
  padding: 10px 15px;
}
</style>
