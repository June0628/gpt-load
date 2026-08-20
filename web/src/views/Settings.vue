<script setup lang="ts">
import {
  settingsApi,
  type LogTableInfo,
  type Setting,
  type SettingCategory,
  type TestLogUploadResult,
} from "@/api/settings";
import ProxyKeysInput from "@/components/common/ProxyKeysInput.vue";
import {
  CloudUploadOutline,
  HelpCircle,
  Save,
  CheckmarkCircle,
  CloseCircleOutline,
} from "@vicons/ionicons5";
import {
  NAlert,
  NButton,
  NCard,
  NEmpty,
  NForm,
  NFormItem,
  NGrid,
  NGridItem,
  NIcon,
  NInput,
  NInputNumber,
  NSelect,
  NSpace,
  NSwitch,
  NText,
  NTooltip,
  useMessage,
  type FormItemRule,
} from "naive-ui";
import { computed, ref } from "vue";
import { useI18n } from "vue-i18n";

const { t } = useI18n();

const settingList = ref<SettingCategory[]>([]);
const formRef = ref();
const form = ref<Record<string, string | number | boolean>>({});
const isSaving = ref(false);
const message = useMessage();

// 手动备份上传相关
const logTables = ref<LogTableInfo[]>([]);
const selectedTable = ref<string | null>(null);
const isUploading = ref(false);

// 日志上传配置测试相关
const testResult = ref<TestLogUploadResult | null>(null);
const isTesting = ref(false);

const logTableOptions = computed(() =>
  logTables.value.map(table => ({
    label: `${table.date}  (${table.row_count.toLocaleString()} ${t("settings.rowsUnit")}, ${table.size_human})`,
    value: table.table_name,
  }))
);

const isLogUploadEnabled = computed(() => form.value["log_upload_enabled"] === true);
const isDeleteAfterManualUpload = computed(
  () => form.value["log_upload_delete_after_manual"] === true
);

fetchSettings();
fetchLogTables();

async function fetchSettings() {
  try {
    const data = await settingsApi.getSettings();
    settingList.value = data || [];
    initForm();
  } catch (_error) {
    message.error(t("settings.loadFailed"));
  }
}

async function fetchLogTables() {
  try {
    logTables.value = await settingsApi.getLogTables();
  } catch (_error) {
    // silently fail - backup section will show empty
  }
}

async function handleUploadBackup() {
  if (!selectedTable.value || isUploading.value) {
    return;
  }
  isUploading.value = true;
  try {
    await settingsApi.uploadLogTable(selectedTable.value);
    selectedTable.value = null;
    await fetchLogTables();
  } catch (_error) {
    // 错误提示已由 axios 响应拦截器处理，此处无需重复弹出
  } finally {
    isUploading.value = false;
  }
}

function initForm() {
  form.value = settingList.value.reduce(
    (acc: Record<string, string | number | boolean>, category) => {
      category.settings?.forEach(setting => {
        acc[setting.key] = setting.value;
      });
      return acc;
    },
    {}
  );
}

async function handleSubmit() {
  if (isSaving.value) {
    return;
  }

  try {
    await formRef.value.validate();
    isSaving.value = true;
    await settingsApi.updateSettings(form.value);
    await fetchSettings();
  } catch (_error) {
    // 表单验证失败或保存失败时，错误提示已由 axios 响应拦截器或 validate 处理
  } finally {
    isSaving.value = false;
  }
}

function generateValidationRules(item: Setting): FormItemRule[] {
  const rules: FormItemRule[] = [];
  if (item.required) {
    const rule: FormItemRule = {
      required: true,
      message: t("settings.pleaseInput", { field: item.name }),
      trigger: ["input", "blur"],
    };
    if (item.type === "int") {
      rule.type = "number";
    }
    rules.push(rule);
  }
  if (item.type === "int" && item.min_value !== undefined && item.min_value !== null) {
    rules.push({
      validator: (_rule: FormItemRule, value: number) => {
        if (value === null || value === undefined) {
          return true;
        }
        if (item.min_value !== undefined && item.min_value !== null && value < item.min_value) {
          return new Error(t("settings.minValueError", { value: item.min_value }));
        }
        return true;
      },
      trigger: ["input", "blur"],
    });
  }
  return rules;
}

// 测试日志上传配置
async function handleTestLogUpload() {
  if (!isLogUploadEnabled.value || isTesting.value) {
    return;
  }

  isTesting.value = true;
  testResult.value = null;

  try {
    // 将当前表单配置传给后端测试（支持未保存的配置）
    const result = await settingsApi.testLogUploadConfig(form.value);
    testResult.value = result;

    // 只在成功时弹出提示，失败时由UI区域显示错误信息
    if (result.success) {
      message.success(t("settings.logUploadTestSuccess"));
    }
  } catch (error: any) {
    console.error("Failed to test log upload config:", error);

    // 设置失败结果以便UI显示
    const errorMessage =
      error?.response?.data?.message || error?.message || t("settings.logUploadTestError");
    testResult.value = {
      success: false,
      provider: (form.value["log_upload_provider"] as string) || "unknown",
      message: errorMessage,
      test_time: new Date().toISOString(),
    };
  } finally {
    isTesting.value = false;
  }
}
</script>

<template>
  <n-space vertical>
    <n-form ref="formRef" :model="form" label-placement="top">
      <n-space vertical>
        <n-card
          size="small"
          v-for="category in settingList"
          :key="category.category_name"
          :title="category.category_name"
          hoverable
          bordered
        >
          <n-grid :x-gap="36" :y-gap="0" responsive="screen" cols="1 s:2 m:2 l:4 xl:4">
            <n-grid-item
              v-for="item in category.settings"
              :key="item.key"
              :span="item.key === 'proxy_keys' ? 3 : 1"
            >
              <n-form-item :path="item.key" :rule="generateValidationRules(item)">
                <template #label>
                  <n-space align="center" :size="4" :wrap-item="false">
                    <n-tooltip trigger="hover" placement="top">
                      <template #trigger>
                        <n-icon
                          :component="HelpCircle"
                          :size="16"
                          style="cursor: help; color: #9ca3af"
                        />
                      </template>
                      {{ item.description }}
                    </n-tooltip>
                    <span>{{ item.name }}</span>
                  </n-space>
                </template>

                <n-input-number
                  v-if="item.type === 'int'"
                  v-model:value="form[item.key] as number"
                  :min="
                    item.min_value !== undefined && item.min_value >= 0 ? item.min_value : undefined
                  "
                  :placeholder="t('settings.inputNumber')"
                  clearable
                  style="width: 100%"
                  size="small"
                />
                <n-switch
                  v-else-if="item.type === 'bool'"
                  v-model:value="form[item.key] as boolean"
                  size="small"
                />
                <proxy-keys-input
                  v-else-if="item.key === 'proxy_keys'"
                  v-model="form[item.key] as string"
                  :placeholder="t('settings.inputContent')"
                  size="small"
                />
                <n-input
                  v-else
                  v-model:value="form[item.key] as string"
                  :placeholder="t('settings.inputContent')"
                  clearable
                  size="small"
                />
              </n-form-item>
            </n-grid-item>
          </n-grid>
        </n-card>
      </n-space>
    </n-form>

    <div
      v-if="settingList.length > 0"
      style="display: flex; justify-content: center; padding-top: 12px"
    >
      <n-button
        type="primary"
        size="large"
        :loading="isSaving"
        :disabled="isSaving"
        @click="handleSubmit"
        style="min-width: 200px"
      >
        <template #icon>
          <n-icon :component="Save" />
        </template>
        {{ isSaving ? t("settings.saving") : t("settings.saveSettings") }}
      </n-button>
    </div>

    <!-- 日志上传配置测试 -->
    <n-card
      v-if="isLogUploadEnabled"
      size="small"
      :title="t('settings.logUploadTest')"
      hoverable
      bordered
      style="margin-top: 16px"
    >
      <n-space vertical>
        <n-text depth="3" style="display: block; margin-bottom: 12px">
          {{ t("settings.logUploadTestDesc") }}
        </n-text>

        <n-space align="center">
          <n-button
            type="info"
            size="small"
            :loading="isTesting"
            :disabled="isTesting"
            @click="handleTestLogUpload"
          >
            {{ t("settings.testLogUpload") }}
          </n-button>
        </n-space>

        <!-- 测试结果显示 -->
        <div v-if="testResult" style="margin-top: 12px">
          <n-alert :type="testResult.success ? 'success' : 'error'" :bordered="true" size="small">
            <template #icon>
              <n-icon :component="testResult.success ? CheckmarkCircle : CloseCircleOutline" size="20" />
            </template>
            <div>
              <div style="margin-bottom: 4px">
                {{ testResult.message }}
              </div>
              <n-text depth="3" style="font-size: 12px">
                {{ t("settings.testTime") }}:
                {{ new Date(testResult.test_time).toLocaleString() }} |
                {{ t("settings.provider") }}:
                {{
                  ["tencent", "cos", "tencent_cos"].includes(testResult.provider)
                    ? t("settings.providerTencent")
                    : testResult.provider === "webdav"
                      ? t("settings.providerWebDAV")
                      : testResult.provider
                }}
              </n-text>
            </div>
          </n-alert>
        </div>
      </n-space>
    </n-card>

    <!-- 手动备份上传 -->
    <n-card
      v-if="isLogUploadEnabled"
      size="small"
      :title="t('settings.logBackup')"
      hoverable
      bordered
      style="margin-top: 16px"
    >
      <n-text depth="3" style="display: block; margin-bottom: 12px">
        {{ t("settings.logBackupDesc") }}
        <n-text v-if="isDeleteAfterManualUpload" type="warning" style="margin-left: 8px">
          ⚠️ {{ t("settings.autoDeleteEnabled") }}
        </n-text>
      </n-text>
      <n-space v-if="logTableOptions.length > 0" align="center">
        <n-select
          v-model:value="selectedTable"
          :options="logTableOptions"
          :placeholder="t('settings.selectTablePlaceholder')"
          style="min-width: 320px"
          size="small"
          clearable
        />
        <n-button
          :type="isDeleteAfterManualUpload ? 'warning' : 'primary'"
          size="small"
          :loading="isUploading"
          :disabled="!selectedTable || isUploading"
          @click="handleUploadBackup"
        >
          <template #icon>
            <n-icon :component="CloudUploadOutline" />
          </template>
          {{
            isUploading
              ? t("settings.uploading")
              : isDeleteAfterManualUpload
                ? t("settings.uploadAndDelete")
                : t("settings.uploadBackup")
          }}
        </n-button>
      </n-space>
      <n-empty v-else :description="t('settings.noTables')" size="small" />
    </n-card>
  </n-space>
</template>
