/* ZCode Proxy i18n：默认跟随系统语言，用户可覆盖且记忆（localStorage）
   机制：中文原文即 key，t()/tf() 在非中文语言下查 I18N_EN，未命中回落中文原文 */
'use strict';

// ---- 语言解析与存储 ----

const LANG_STORAGE_KEY = 'zcode_lang';
let LANG_CHOICE = 'auto';   // 用户显式选择：'auto' | 'zh' | 'en'
let CURRENT_LANG = 'zh';    // 实际生效语言：'zh' | 'en'

function detectSystemLang() {
  // 按偏好顺序做首个命中匹配（支持 zh/en），尊重系统语言优先级而非"列表里出现中文就算中文"
  const list = (navigator.languages && navigator.languages.length) ? navigator.languages : [navigator.language || 'en'];
  for (const l of list) {
    if (!l) continue;
    if (/^zh/i.test(l)) return 'zh';
    if (/^en/i.test(l)) return 'en';
  }
  return 'en';
}

function resolveLang() {
  try {
    const c = localStorage.getItem(LANG_STORAGE_KEY);
    if (c === 'zh' || c === 'en') return c;
  } catch (e) { /* 隐私模式等场景下 localStorage 不可用 */ }
  return detectSystemLang();
}

function currentLocale() {
  return CURRENT_LANG === 'zh' ? 'zh-CN' : 'en-US';
}

// t 纯文本翻译；tf 先翻译再依次把 %s 换成参数（用于带插值的串）
function t(s) {
  if (CURRENT_LANG !== 'en') return s;
  const v = I18N_EN[s];
  return v === undefined ? s : v;
}
function tf(key, ...args) {
  let s = t(key);
  for (const a of args) {
    // 用函数替换：参数（多为服务端错误文本）里的 $&、$` 等不得被当作替换模式展开
    s = s.replace('%s', () => String(a));
  }
  return s;
}

// ---- 静态 DOM 应用：data-i18n=textContent / data-i18n-html=innerHTML / data-i18n-ph=placeholder / data-i18n-title=title ----

function applyI18n() {
  document.documentElement.lang = CURRENT_LANG === 'zh' ? 'zh-CN' : 'en';
  document.querySelectorAll('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
  document.querySelectorAll('[data-i18n-html]').forEach(el => { el.innerHTML = t(el.dataset.i18nHtml); });
  document.querySelectorAll('[data-i18n-ph]').forEach(el => { el.placeholder = t(el.dataset.i18nPh); });
  document.querySelectorAll('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
  document.querySelectorAll('.lang-select').forEach(sel => { sel.value = LANG_CHOICE; });
}

// setLangChoice 语言切换入口：显式选择持久化，auto 清除记忆回到跟随系统
function setLangChoice(v) {
  LANG_CHOICE = (v === 'zh' || v === 'en') ? v : 'auto';
  try {
    if (LANG_CHOICE === 'auto') localStorage.removeItem(LANG_STORAGE_KEY);
    else localStorage.setItem(LANG_STORAGE_KEY, LANG_CHOICE);
  } catch (e) { /* ignore */ }
  CURRENT_LANG = LANG_CHOICE === 'auto' ? detectSystemLang() : LANG_CHOICE;
  applyI18n();
  // 主界面可见时重载当前分区，让已渲染的动态内容也换成新语言
  if (typeof onLanguageChanged === 'function') onLanguageChanged();
}

// ---- 英文词典（key = 中文原文；新界面文案请同步补充） ----

const I18N_EN = {
  // 通用
  '加载中…': 'Loading…',
  '刷新': 'Refresh',
  '关闭': 'Close',
  '取消': 'Cancel',
  '保存': 'Save',
  '删除': 'Delete',
  '编辑': 'Edit',
  '复制': 'Copy',
  '测试': 'Test',
  '使用': 'Use',
  '说明': 'Notes',
  '状态': 'Status',
  '账号': 'Account',
  '分组': 'Group',
  '操作': 'Actions',
  '名称': 'Name',
  '类型': 'Type',
  '地址': 'Address',
  '测试失败: %s': 'Test failed: %s',
  '请选择目标账号': 'Please select a target account',
  '打开授权页': 'Open authorization page',
  '账号列表加载失败: %s': 'Failed to load account list: %s',
  '当前环境不支持自动复制，请手动复制': 'Automatic copy is not supported here; please copy manually',
  '复制失败，请手动复制': 'Copy failed; please copy manually',
  '检测': 'Check',
  '耗时': 'Duration',
  '启用': 'Enabled',
  '停用': 'Disabled',
  '默认': 'Default',
  '是': 'Yes',
  '否': 'No',
  '成功': 'Success',
  '失败': 'Failed',
  '时间': 'Time',
  '无': 'None',
  '备注': 'Note',
  '主机': 'Host',
  '端口': 'Port',
  '协议': 'Protocol',
  '模型': 'Model',
  '模式': 'Mode',
  '运行中…': 'Running…',
  '测试中…': 'Testing…',
  '已保存': 'Saved',
  '已删除': 'Deleted',
  '已复制': 'Copied',
  '不可用': 'Unavailable',
  '未登录或会话过期': 'Not signed in or session expired',
  '登录失败': 'Sign-in failed',
  '❌ %s': '❌ %s',
  '通用': 'Universal',
  '导入': 'Import',

  // 相对时间
  '从未': 'never',
  '%s秒前': '%ss ago',
  '%s分钟前': '%sm ago',
  '%s小时前': '%sh ago',
  '%s天前': '%sd ago',

  // 账号状态
  '正常': 'Active',
  '冷却中': 'Cooling down',
  '额度耗尽': 'Quota exhausted',
  '凭证失效': 'Credentials invalid',
  '未激活': 'Not activated',
  '已禁用': 'Disabled',

  // 登录页
  '多账号管理 · 2API 网关': 'Multi-account management · 2API gateway',
  '用户名': 'Username',
  '密码': 'Password',
  '登录': 'Sign in',
  '默认账户 admin，初始密码见服务端首次启动日志（或设置 ZCODE_WEB_PASS）': 'Default user admin; the initial password is printed once in the server log on first start (or set ZCODE_WEB_PASS).',
  '当前使用初始管理口令，请尽快在「设置 → 账号安全」中修改': 'The initial admin password is still in use; please change it soon under Settings → Account Security.',
  '当前目标，列表加载失败': 'current target (list failed to load)',
  '当前目标（账号已删除）': 'current target (account deleted)',
  '若授权后无法自动跳回（当前上游常报 Redirect URI 未注册），请点「取消」，把模式切换为「手动粘贴」后重新发起登录并提交新授权页跳转的完整 URL——旧授权码不可复用。': 'If the redirect back fails (the upstream often reports "Redirect URI not registered"), click Cancel, switch the mode to "Manual paste", start a new login, and submit the full URL of the NEW authorization redirect — old authorization codes cannot be reused.',

  // 语言选择器
  '跟随系统': 'System default',

  // 顶栏 / 导航
  '仪表盘': 'Dashboard',
  '账号管理': 'Accounts',
  '活动任务': 'Campaign tasks',
  '使用记录': 'Usage',
  'LLM 测试': 'LLM Test',
  '设置': 'Settings',
  'API 文档': 'API Docs',
  '更多操作': 'More actions',
  '账号导入': 'Import accounts',
  '导入本地客户端账号': 'Import local client account',
  'OAuth 登录新账号': 'OAuth login',
  '粘贴 JWT / API Key': 'Paste JWT / API Key',
  '迁移': 'Migration',
  '导出加密账号包': 'Export encrypted bundle',
  '导入加密账号包': 'Import encrypted bundle',
  '同步': 'Sync',
  '刷新所有额度': 'Refresh all quotas',
  '检测全部活动': 'Detect all campaigns',
  '运行中': 'Running',
  '退出': 'Sign out',

  // 仪表盘
  '总账号数': 'Total accounts',
  '可用账号': 'Available accounts',
  '总剩余额度': 'Total remaining quota',
  '7日请求数': 'Requests (7d)',
  '7日 Tokens': 'Tokens (7d)',
  '平均首字延迟': 'Avg TTFT',
  '系统状态': 'System status',
  '账号状态分布': 'Account status breakdown',
  '客户端版本伪装': 'Client version spoof',
  '验证码参数': 'Captcha param',
  '✅ 新鲜': '✅ Fresh',
  '⏳ 宽限期(%ss)': '⏳ Grace period (%ss)',
  '— 未求解': '— Not solved',
  '验证码模式': 'Captcha mode',
  '有头手动': 'Headed (manual)',
  '无头自动': 'Headless (auto)',
  '验证码配置': 'Captcha config',
  '上游已启用 scene=%s': 'Upstream enabled, scene=%s',
  '上游未启用': 'Upstream not enabled',
  '未获取': 'Not fetched',

  // 分组
  '分组筛选': 'Filter by group',
  '全部分组': 'All groups',
  '未分组': 'Ungrouped',
  '＋ 新分组…': '＋ New group…',
  '新分组名称:': 'New group name:',

  // 账号列表
  '+ OAuth 登录': '+ OAuth login',
  '导入本地账号': 'Import local account',
  '粘贴导入': 'Paste import',
  '刷新额度': 'Refresh quotas',
  '账号列表': 'Account list',
  '暂无账号，点击「导入本地账号」或「OAuth 登录」开始': 'No accounts yet — start with "Import local account" or "OAuth login"',
  '认证': 'Auth',
  '套餐 / 剩余额度': 'Plan / remaining quota',
  '设备指纹': 'Device fingerprint',
  '使用/失败': 'Used/Failed',
  '最近活动': 'Last claim',
  '备注: %s': 'Note: %s',
  '+APIKey回退': '+API key fallback',
  '点击查看套餐与额度构成': 'View plan and quota details',
  '未刷新': 'Not refreshed',
  '%s 到期': 'Expires %s',
  '更多 ▾': 'More ▾',
  '领活动': 'Claim',

  // 额度弹窗
  '套餐与额度构成 · %s': 'Plan & quota breakdown · %s',
  '暂无额度数据（%s）': 'No quota data (%s)',
  '凭证鉴权失败（401/403），请重新登录该账号。': 'Credential auth failed (401/403) — please sign in to this account again.',
  '该账号无 Coding Plan / 未激活。': 'This account has no Coding Plan / not activated.',
  '点击账号行「刷新」拉取额度后再查看。': 'Use "Refresh" on the account row to fetch quota first.',
  '套餐': 'Plan',
  '套餐档位': 'Plan tier',
  '到期时间': 'Expiry',
  '总 / 已用 / 剩余': 'Total / Used / Remaining',
  '使用占比': 'Used',
  '数据源': 'Source',
  '刷新时间': 'Refreshed at',
  '到期 %s': 'Expires %s',
  '总 %s · 已用 %s · 剩余 %s（%s%）': 'Total %s · Used %s · Remaining %s (%s%)',
  '模型 / 权益': 'Model / benefit',
  '总额': 'Total',
  '已用': 'Used',
  '剩余': 'Remaining',
  '占比': 'Usage',
  '周期 / 到期': 'Period / expiry',
  '该套餐无明细额度项': 'No quota items in this plan',

  // 账号操作
  '账号操作 · %s': 'Account actions · %s',
  '🔍 检测活动': '🔍 Detect campaigns',
  '⚡ 激活套餐': '⚡ Activate plan',
  '♻️ 配额重置（Coding Plan）': '♻️ Quota reset (Coding Plan)',
  '✏️ 编辑分组 / 备注': '✏️ Edit group / note',
  '💾 切回本地客户端': '💾 Switch back to local client',
  '↩️ 从快照还原本地': '↩️ Restore local from snapshot',
  '🗑 删除账号': '🗑 Delete account',

  // 账号动作 toasts
  '额度已刷新': 'Quota refreshed',
  '刷新失败: %s': 'Refresh failed: %s',
  '正在刷新所有账号额度…': 'Refreshing quotas for all accounts…',
  '全部刷新完成': 'All quotas refreshed',
  '正在检测并领取活动（含验证码求解，约 10-30 秒）…': 'Detecting and claiming (captcha solving included, ~10-30s)…',
  '领取成功: %s': 'Claimed: %s',
  '领取未成功: %s': 'Claim not successful: %s',
  '领取失败: %s': 'Claim failed: %s',
  '活动检测': 'Campaign detection',
  '优先级 %s': 'Priority %s',
  '当前无可领取活动': 'No claimable campaigns right now',
  '立即领取最高优先级': 'Claim highest priority now',
  '检测失败: %s': 'Detection failed: %s',
  '正在检测 %s 个账号的活动…': 'Detecting campaigns on %s accounts…',
  '检测完成：%s/%s 个账号发现活动': 'Done: campaigns found on %s/%s accounts',
  '正在查询重置机会…': 'Checking reset opportunities…',
  '配额重置': 'Quota reset',
  '该接口仅付费 Coding Plan 账号可用（Start Plan 返回 3101 coding plan is required）。': 'Only paid Coding Plan accounts can use this (Start Plan returns 3101 coding plan is required).',
  '配额重置机会': 'Quota reset opportunities',
  '5 小时窗口重置': '5-hour window resets',
  '%s 次可用': '%s available',
  '周重置': 'Weekly resets',
  '最近 5h 重置': 'Last 5h reset',
  '最近周重置': 'Last weekly reset',
  '消耗一次机会立即恢复对应窗口配额（优先 five_hour）。': 'One opportunity instantly restores the matching window quota (five_hour first).',
  '执行重置': 'Run reset',
  '正在执行配额重置…': 'Running quota reset…',
  '重置成功': 'Reset successful',
  '重置失败: %s': 'Reset failed: %s',
  '正在上报激活事件…': 'Reporting activation event…',
  '激活成功: %s': 'Activated: %s',
  '激活未完成: %s': 'Activation incomplete: %s',
  '激活失败: %s': 'Activation failed: %s',
  '编辑账号': 'Edit account',
  '启用（参与轮询）': 'Enabled (in rotation)',
  '确认删除账号 %s？此操作不可恢复。': 'Delete account %s? This cannot be undone.',
  '一键切回本地客户端': 'Switch back to local client',
  '将把账号 <b>%s</b> 的凭证重新加密写回本机<span class="mono">~/.zcode/v2/credentials.json</span> 与 <span class="mono">config.json</span>（原文件自动备份到 data/backups/）。': 'Re-encrypts and writes the credentials of account <b>%s</b> back to the local <span class="mono">~/.zcode/v2/credentials.json</span> and <span class="mono">config.json</span> (originals are backed up to data/backups/).',
  '写回后结束 ZCode.exe 进程（下次启动生效）': 'Kill the ZCode.exe process after writing (takes effect on next start)',
  '确认切回': 'Confirm switch back',
  '已切回本地客户端': 'Switched back to local client',
  '切回失败: %s': 'Switch back failed: %s',
  '用该账号导入时的快照覆盖本地客户端当前登录态？': 'Overwrite the current local client login state with the snapshot taken when this account was imported?',
  '已还原': 'Restored',
  '还原失败: %s': 'Restore failed: %s',

  // 导入
  '正在从本地 ZCode 客户端导入…': 'Importing from the local ZCode client…',
  '导入成功: %s': 'Imported: %s',
  '导入失败: %s': 'Import failed: %s',
  '导入成功': 'Import successful',
  '通道': 'Channel',
  '名称（可选）': 'Name (optional)',
  '账号备注名': 'account display name',
  '凭证（JWT 或 API Key）': 'Credential (JWT or API key)',
  'eyJhbGci… 三段 JWT，或 xxx.yyy 格式 API Key': 'eyJhbGci… three-segment JWT, or an xxx.yyy API key',

  // OAuth
  '方式': 'Method',
  '手动粘贴（推荐：Z.AI 仅注册了 zcode.z.ai/login 回跳）': 'Manual paste (recommended: Z.AI only registered the zcode.z.ai/login redirect)',
  '环回自动（实验：当前会报 Redirect URI not registered）': 'Loopback auto (experimental: currently fails with "Redirect URI not registered")',
  '开始登录': 'Start login',
  '发起登录失败: %s': 'Failed to start login: %s',
  '已在新标签页打开 Z.AI 授权页，登录并授权后自动跳回本网关完成入库。': 'Opened the Z.AI authorization page in a new tab — after login and approval it redirects back to this gateway and the account is imported.',
  '已在新标签页打开 Z.AI 授权页。步骤：① 登录并同意授权 → ② 浏览器会跳到 <b>zcode.z.ai/login?code=…</b> → ③ 复制该地址栏<b>完整 URL</b>粘贴到下面 → ④ 提交兑换。': 'Opened the Z.AI authorization page in a new tab. Steps: ① log in and approve → ② the browser lands on <b>zcode.z.ai/login?code=…</b> → ③ copy the <b>full URL</b> from the address bar and paste it below → ④ submit to redeem.',
  '粘贴回跳 URL（或仅 code）': 'Paste the redirect URL (or just the code)',
  '提交兑换': 'Redeem',
  '请粘贴回跳 URL 或 code': 'Paste the redirect URL or code first',
  '兑换中…': 'Redeeming…',
  '登录成功，账号已入库': 'Login successful — account imported',
  '登录成功: %s': 'Login successful: %s',
  '正在兑换 token 并提取 API Key…': 'Exchanging token and extracting the API key…',

  // 活动计划
  '活动计划（cron 调度）': 'Campaign plans (cron scheduling)',
  '+ 新建计划': '+ New plan',
  '暂无计划，点击「+ 新建计划」创建': 'No plans yet — click "+ New plan" to create one',
  '领取记录': 'Claim records',
  '计划运行记录': 'Plan run history',
  '暂无领取记录': 'No claim records',
  '暂无运行记录': 'No run records',
  '检测活动': 'Detect campaigns',
  '一键领取': 'One-click claim',
  '激活套餐': 'Activate plan',
  '计划名': 'Plan name',
  '任务': 'Task',
  '目标': 'Target',
  '间隔': 'Interval',
  '下次运行': 'Next run',
  '最近运行': 'Last run',
  '账号#%s': 'Account #%s',
  '分组: %s': 'Group: %s',
  '全部账号': 'All accounts',
  '立即运行': 'Run now',
  '编辑计划': 'Edit plan',
  '新建计划': 'New plan',
  '计划名称': 'Plan name',
  '例如：每日领取活动': 'e.g. Daily campaign claim',
  '任务类型': 'Task type',
  '一键领取（检测+验证码+领取）': 'One-click claim (detect + captcha + claim)',
  '仅检测活动': 'Detect campaigns only',
  '激活套餐（上报激活事件）': 'Activate plan (report activation event)',
  '配额重置（耗尽时恢复窗口配额）': 'Quota reset (restore window quota when exhausted)',
  'cron 表达式（分 时 日 月 周）': 'cron expression (min hour day month weekday)',
  '示例：0 9 * * * = 每天 09:00；*/30 * * * * = 每 30 分钟': 'Examples: 0 9 * * * = daily at 09:00; */30 * * * * = every 30 minutes',
  '全部可用账号': 'All available accounts',
  '指定分组': 'Specific group',
  '单个账号': 'Single account',
  '选择账号': 'Select account',
  '账号间隔（秒，防风控，含 0~50% 随机抖动）': 'Account interval (seconds, anti-risk-control, 0-50% random jitter)',
  '未命名计划': 'Untitled plan',
  '计划已保存': 'Plan saved',
  '确认删除该计划？': 'Delete this plan?',
  '已开始执行，见顶部进度': 'Started — see progress at the top',
  '▶ %s（%s）': '▶ %s (%s)',
  '活动': 'Campaign',
  '结果': 'Result',
  '信息': 'Info',
  '运行时间': 'Run time',
  '计划': 'Plan',
  '成功/失败': 'Success/Fail',
  '摘要': 'Summary',

  // 使用记录
  '7日请求': 'Requests (7d)',
  '输入 Tokens': 'Input tokens',
  '输出 Tokens': 'Output tokens',
  '平均耗时': 'Avg duration',
  '平均 TTFT': 'Avg TTFT',
  '暂无使用记录': 'No usage records',
  '输入': 'In',
  '输出': 'Out',
  '合计': 'Total',
  '流式': 'Stream',

  // 使用统计（成功率 / 缓存 / 按天趋势）
  '缓存命中': 'Cache read',
  '成功率': 'Success rate',
  '缓存命中率': 'Cache hit rate',
  'TTFT P50 / P95': 'TTFT P50 / P95',
  '按网关 Key 与按天统计（7 日）': 'By gateway key & daily trend (7d)',
  '请求数': 'Requests',
  '日期': 'Date',
  '暂无数据': 'No data',

  // 网关 Key
  '网关 Key / API Keys': 'Gateway Keys / API Keys',
  '+ 新建 Key': '+ New key',
  '暂无网关 Key，点击「+ 新建 Key」创建': 'No gateway keys yet — click "+ New key" to create one',
  'Key 前缀': 'Key prefix',
  '配额': 'Quota',
  '最近使用': 'Last used',
  '不限': 'Unlimited',
  '全部': 'All',
  '新建网关 Key': 'New gateway key',
  '编辑网关 Key': 'Edit gateway key',
  '例如：ci-机器人': 'e.g. ci-bot',
  'RPM 限制（0=不限）': 'RPM limit (0 = unlimited)',
  '0=不限': '0 = unlimited',
  '总配额（tokens，0=不限）': 'Total quota (tokens, 0 = unlimited)',
  '模型白名单（逗号分隔，留空=全部）': 'Model whitelist (comma-separated, empty = all)',
  'glm-5.3,glm-5.2 留空=全部': 'glm-5.3,glm-5.2 — empty = all',
  '请填写名称': 'Name is required',
  '网关 Key 已创建': 'Gateway key created',
  '请立即保存明文 Key，关闭后不再显示！': 'Save the plaintext key now — it will NOT be shown again after this dialog closes!',
  '我已保存，关闭': 'Saved — close',
  '确认删除网关 Key %s？此操作不可恢复。': 'Delete gateway key %s? This cannot be undone.',
  '已启用': 'Enabled',
  '已停用': 'Disabled',

  // LLM 测试
  'LLM 连通性测试': 'LLM connectivity test',
  '刷新 Key': 'Refresh key',
  '流式（SSE）': 'Streaming (SSE)',
  '非流式': 'Non-streaming',
  '提示词': 'Prompt',
  '输入要发送给模型的提示词…': 'Enter the prompt to send to the model…',
  '用一句话介绍你自己。': 'Introduce yourself in one sentence.',
  '用三行诗描述秋天。': 'Describe autumn in a three-line poem.',
  '把下面句子翻译成英文：今天天气真好。': 'Translate into English: The weather is really nice today.',
  '解释什么是大语言模型，限 50 字。': 'Explain what a large language model is, in 50 words.',
  '1+1=？只回答数字。': '1+1=? Reply with the number only.',
  '自我介绍': 'Self intro',
  '写诗': 'Poem',
  '翻译': 'Translate',
  '概念解释': 'Concept',
  '极简问答': 'Minimal QA',
  '网关 Key': 'Gateway key',
  '运行测试': 'Run test',
  '测试结果': 'Test result',
  '清空历史': 'Clear history',
  '响应内容': 'Response body',
  '历史（本次会话）': 'History (this session)',
  '（尚未运行）': '(not run yet)',
  '（无）': '(none)',
  '你好': 'Hello',
  '缺少网关 Key，点击「刷新 Key」': 'Missing gateway key — click "Refresh key"',
  'HTTP 状态': 'HTTP status',
  '网络错误': 'network error',
  '总延迟': 'Total latency',
  '首字 TTFT': 'TTFT',
  'SSE 事件数': 'SSE events',
  '请求失败: %s': 'Request failed: %s',
  '【模型仅输出思考过程（max_tokens 不足或未产出正文）】': '[Model output thinking only — max_tokens too small or no body produced]',
  '（空响应）': '(empty response)',
  '测试完成': 'Test complete',
  '测试返回 %s': 'Test returned %s',

  // 设置页签
  '账号安全': 'Security',
  '账户策略': 'Strategy',
  '出口代理': 'Egress proxy',
  '指纹伪装': 'Fingerprint',
  '人机验证': 'Captcha',
  '可用模型': 'Models',

  // 安全
  '修改密码': 'Change password',
  '旧密码': 'Old password',
  '新密码': 'New password',
  '网关 API Key（/v1 接口鉴权）': 'Gateway API key (/v1 auth)',
  '当前 Key': 'Current key',
  '尚未生成': 'Not generated yet',
  '重新生成': 'Regenerate',
  '客户端配置：base_url=http://127.0.0.1:8687/v1，api-key 填此 Key': 'Client config: base_url=http://127.0.0.1:8687/v1 with api-key set to this key',
  '重新生成后旧 Key 立即失效，确认？': 'Regenerating invalidates the old key immediately. Continue?',
  '已生成新 API Key': 'New API key generated',
  '密码已修改，请重新登录': 'Password changed — please sign in again',

  // 策略
  '账号选择策略': 'Account selection strategy',
  '轮询策略': 'Selection strategy',
  '轮询（round_robin）': 'Round robin (round_robin)',
  '随机（random）': 'Random (random)',
  '剩余额度优先（best_quota）': 'Best remaining quota (best_quota)',
  '额度自动刷新间隔（秒，0=关闭）': 'Quota auto-refresh interval (seconds, 0 = off)',
  '保存策略': 'Save strategy',
  '策略已保存': 'Strategy saved',
  '系统提示词缓存断点': 'Prompt cache breakpoint',
  '在 system 末块标记 cache_control，命中上游提示缓存后按缓存价计费；上游不支持时可关闭': 'Marks the last system block with cache_control — once the upstream prompt cache hits, cached tokens are billed at cache rates; turn it off if the upstream does not support it.',
  '自动重置（耗尽时按阈值消耗重置机会）': 'Auto reset (spend reset slots on exhaustion, threshold-based)',
  '开启后：配额耗尽且自然重置等待超过阈值时自动消耗重置机会；默认关闭，重置仍可随时手动触发': 'When on: if quota is exhausted and the natural window reset is further away than the threshold, a reset slot is spent automatically; off by default — manual resets always available',
  '5小时窗口阈值（分钟）：自然重置等待小于该值不消耗重置': '5-hour window threshold (minutes): never spend a reset when the natural reset is closer',
  '周窗口阈值（小时）：自然重置等待小于该值不消耗重置': 'Weekly window threshold (hours): never spend a reset when the natural reset is closer',
  '优先级级联（priority：数值小者先用，耗尽自动落到下一层）': 'Priority cascade (priority: lower values are used first; exhausted tiers fall through automatically)',
  '会话粘滞': 'Sticky sessions',
  '同一会话固定同一账号（保住上游 prompt 缓存，更省额度）': 'Keep each conversation on the same account (preserves the upstream prompt cache, saves quota)',
  '每账号并发上限（1-32，默认 3；排队而非打满并发触发 1302 限流）': 'Per-account concurrency cap (1-32, default 3; queues requests instead of tripping upstream 1302 concurrency limits)',
  '双通道额度构成': 'Dual-channel quota breakdown',
  '已耗尽': 'Exhausted',
  '有余量': 'Has headroom',
  '剩余 %s': 'Remaining %s',
  '重置 %s': 'resets at %s',
  'Priority（priority 策略：数值小者先用，促销层默认 50）': 'Priority (priority strategy: lower value is used first; promo/free tier defaults to 50)',
  '状态机说明': 'State machine reference',
  '正常，参与轮询': 'Normal — participates in rotation',
  '限流/风控冷却，到期自动恢复': 'Rate-limited / risk-control cooldown — auto-recovers on expiry',
  '额度用完，额度刷新后恢复': 'Quota exhausted — recovers after quota refresh',
  '凭证失效（401/403），需重新登录': 'Credentials invalid (401/403) — re-login required',
  '套餐未激活，可尝试「激活套餐」': 'Plan not activated — try "Activate plan"',
  '手动禁用': 'Manually disabled',

  // 出口代理
  '出口代理节点（组绑定）': 'Egress proxy nodes (group-bound)',
  '探测本机端口': 'Probe local ports',
  '读取系统代理': 'Read system proxy',
  '+ 添加节点': '+ Add node',
  '暂无代理节点（未配置时全部直连）': 'No proxy nodes (all direct connections when unconfigured)',
  '绑定分组': 'Bound groups',
  '全局上游代理（未绑定组的账号走这里）': 'Global upstream proxy (used by accounts not bound to a group)',
  'http://127.0.0.1:7897 或 socks5://host:port，留空直连': 'http://127.0.0.1:7897 or socks5://host:port — leave empty for direct',
  '全局代理已保存': 'Global proxy saved',
  '出口 IP: %s（%sms）': 'Exit IP: %s (%sms)',
  '✅ 出口 IP: <b>%s</b>（%sms）': '✅ Exit IP: <b>%s</b> (%sms)',
  '系统未启用代理': 'No system proxy enabled',
  '已填入系统代理: %s': 'System proxy filled in: %s',
  '未发现开放的本机代理端口': 'No open local proxy ports found',
  '本机代理端口探测': 'Local proxy port probe',
  '编辑代理节点': 'Edit proxy node',
  '添加代理节点': 'Add proxy node',
  '例如：住宅代理-美国': 'e.g. Residential proxy - US',
  '用户名（可选）': 'Username (optional)',
  '密码（可选）': 'Password (optional)',
  '留空保持不变': 'leave empty to keep unchanged',
  '绑定分组（逗号分隔，留空=不绑定）': 'Bound groups (comma-separated, empty = unbound)',
  '设为默认节点（未绑定组的账号走它）': 'Set as default node (used by accounts without a group)',
  '确认删除该代理节点？': 'Delete this proxy node?',

  // TLS 指纹
  'TLS 指纹（ClientHello 伪装）': 'TLS fingerprint (ClientHello spoofing)',
  '指纹模式': 'Fingerprint mode',
  '自定义 JA3 字符串': 'Custom JA3 string',
  '保存指纹设置': 'Save fingerprint settings',
  '指纹设置已保存（新连接生效）': 'Fingerprint settings saved (applies to new connections)',
  '当前生效: %s（保存后新连接生效）': 'Currently active: %s (new connections after saving)',
  '上游阿里云 ESA WAF 会检查 TLS ClientHello 指纹（JA3）。ZCode 桌面端是 Electron/Chromium，默认 Chrome 伪装与其一致；被风控时可换 firefox/随机化尝试。': 'The upstream Aliyun ESA WAF checks the TLS ClientHello fingerprint (JA3). The ZCode desktop client is Electron/Chromium, so the default Chrome spoof matches it; if risk-controlled, try firefox/randomized.',
  '· 指纹作用于所有上游请求（额度/活动/聊天转发）的 TLS 握手层。': '· The fingerprint applies to the TLS handshake of all upstream requests (quota / campaigns / chat relay).',
  '· 自定义 JA3 为 5 段格式：版本,密码套件,扩展,椭圆曲线,点格式；未识别扩展会跳过。': '· Custom JA3 is 5-segment: version,ciphers,extensions,curves,point formats; unrecognized extensions are skipped.',
  '· HTTP/2 已禁用（ESA WAF 不支持 h2 ALPN），与桌面客户端实测行为一致。': '· HTTP/2 is disabled (ESA WAF does not support h2 ALPN) — matches measured desktop client behavior.',

  // 人机验证
  '人机验证（阿里云无痕）': 'Captcha (Aliyun invisible)',
  '求解模式': 'Solving mode',
  '自动（无头浏览器求解，失败升级有头手动）': 'Auto (headless browser solve; escalates to headed manual on failure)',
  '手动（始终弹出浏览器窗口人工过）': 'Manual (always opens a browser window for a human to solve)',
  '关闭（直连不带验证参数）': 'Off (direct connection without captcha params)',
  '验证码设置已保存': 'Captcha settings saved',
  '立即求解测试': 'Solve test now',
  '失效缓存': 'Invalidate cache',
  '当前参数: %s': 'Current param: %s',
  '⏳ 已过期 %ss': '⏳ expired %ss ago',
  '当前无缓存参数（下次请求时自动求解）': 'No cached params (solved automatically on next request)',
  '正在求解验证码（无头浏览器约 5-15 秒）…': 'Solving captcha (headless browser, ~5-15s)…',
  '求解成功（参数 %s 字节）': 'Solved (%s-byte param)',
  '求解未返回参数': 'Solver returned no param',
  '求解失败: %s': 'Solve failed: %s',
  '缓存已失效': 'Cache invalidated',
  '· zcode.z.ai 免费额度通道需要 <b>X-Aliyun-Captcha-Verify-Param</b>，由本机真实 Chrome/Edge 无头求解（捆绑 Chromium 会被风控识别）。': '· The zcode.z.ai free-quota channel requires <b>X-Aliyun-Captcha-Verify-Param</b>, solved headlessly by a real local Chrome/Edge (a bundled Chromium is detected by risk control).',
  '· 参数缓存 45 秒，过期后 5 分钟宽限期内复用旧参数并后台刷新。': '· Params are cached for 45 seconds; for 5 minutes after expiry the old param is reused while a refresh runs in the background.',
  '· 数据中心 IP 会被风控（F001 环境风险），建议在「出口代理」配置住宅代理。': '· Datacenter IPs are risk-controlled (F001 environment risk) — configure a residential proxy under "Egress proxy".',
  '· 自动模式连续失败 2 次会弹出有头浏览器窗口，人工完成验证后参数同样进入缓存。': '· In auto mode, 2 consecutive failures open a headed browser window; params from the manual solve are cached as well.',

  // 可用模型
  '网关公布模型（/v1/models）': 'Gateway advertised models (/v1/models)',
  '模型清单（逗号分隔，留空使用 config/config.json）': 'Model list (comma-separated, empty = use config/config.json)',
  '模型清单已保存': 'Model list saved',
  '当前生效: ': 'Currently active: ',
  '同步官方目录（client/configs）': 'Sync official catalog (client/configs)',
  '应用目录到网关清单': 'Apply catalog to gateway list',
  '官方目录: ': 'Official catalog: ',
  '正在同步官方模型目录…': 'Syncing the official model catalog…',
  '已同步 %s 个模型': 'Synced %s models',
  '目录为空，请先同步': 'Catalog is empty — sync first',
  '已将目录应用为网关模型清单': 'Catalog applied as the gateway model list',
  '尚未同步，点击「同步官方目录」': 'Not synced yet — click "Sync official catalog"',

  // 账号包迁移
  '导出全部账号的凭证（JWT / API Key / 设备指纹 / 本地快照），PBKDF2+AES-256-GCM 加密，可跨机器导入。': 'Exports all account credentials (JWT / API key / device fingerprint / local snapshot), encrypted with PBKDF2+AES-256-GCM, importable on other machines.',
  '加密密码': 'Encryption password',
  '导入时需要相同密码': 'needed again at import time',
  '管理员密码（确认身份）': 'Admin password (verify identity)',
  '生成账号包': 'Generate bundle',
  '请设置密码': 'Set a password first',
  '请输入管理员密码': 'Enter the admin password',
  '账号包已生成': 'Bundle generated',
  '解密密码': 'Decryption password',
  '账号包内容（zcb1: 开头）': 'Bundle content (starts with zcb1:)',
  '导入成功：%s 个账号': 'Imported %s accounts',

  // API 文档
  '2API 端点': '2API endpoints',
  '端点': 'Endpoint',
  '鉴权：<code>Authorization: Bearer sk-xxx</code> 或 <code>x-api-key: sk-xxx</code>（在「设置 → 账号安全」生成）': 'Auth: <code>Authorization: Bearer sk-xxx</code> or <code>x-api-key: sk-xxx</code> (generate under "Settings → Security")',
  '原生协议直通，支持 SSE 流式': 'Native protocol passthrough with SSE streaming',
  '自动转换为 Anthropic 上游请求，支持 tools / 流式 / include_usage': 'Auto-converts to Anthropic upstream requests; supports tools / streaming / include_usage',
  'Codex / 新版 OpenAI SDK 兼容，含 function_call 事件流': 'Compatible with Codex / newer OpenAI SDKs, including function_call event streams',
  '模型清单（同时带 OpenAI 与 Anthropic 字段）': 'Model list (carries both OpenAI and Anthropic fields)',
  '可选请求头：<code>x-zcode-group: 组名</code> 限定只用某分组的账号。': 'Optional header: <code>x-zcode-group: group-name</code> restricts routing to accounts in that group.',
  '模型名大小写不敏感（glm-5.3 → GLM-5.3），支持 <code>bigmodel/</code> 前缀路由到 BigModel 通道。': 'Model names are case-insensitive (glm-5.3 → GLM-5.3); the <code>bigmodel/</code> prefix routes to the BigModel channel.',
  'curl 示例': 'curl example',
  'curl http://127.0.0.1:8687/v1/chat/completions \\\n  -H "Authorization: Bearer sk-你的KEY" \\\n  -H "Content-Type: application/json" \\\n  -d \'{"model":"GLM-4.5-Flash","messages":[{"role":"user","content":"你好"}],"stream":true}\'': 'curl http://127.0.0.1:8687/v1/chat/completions \\\n  -H "Authorization: Bearer sk-YOUR_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d \'{"model":"GLM-4.5-Flash","messages":[{"role":"user","content":"Hello"}],"stream":true}\'',
};

// ---- 启动：解析语言并应用到静态 DOM（脚本位于 body 末尾，DOM 已就绪） ----

LANG_CHOICE = (() => {
  try {
    const c = localStorage.getItem(LANG_STORAGE_KEY);
    return (c === 'zh' || c === 'en') ? c : 'auto';
  } catch (e) { return 'auto'; }
})();
CURRENT_LANG = LANG_CHOICE === 'auto' ? detectSystemLang() : LANG_CHOICE;
applyI18n();
