/* ZCode Proxy 管理界面（文案经 i18n.js 的 t()/tf()，默认语言跟随系统、可覆盖且记忆） */
'use strict';

// ---- 基础设施 ----

async function api(path, opts = {}) {
  const init = {
    method: opts.method || 'GET',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
  };
  if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
  const resp = await fetch(path, init);
  let data = null;
  try { data = await resp.json(); } catch (e) { /* empty */ }
  if (resp.status === 401 && !path.startsWith('/api/login')) {
    showLogin();
    const msg = (data && (data.error?.message || data.error)) || t('未登录或会话过期');
    throw new Error(typeof msg === 'string' ? msg : t('未登录或会话过期'));
  }
  if (!resp.ok) {
    const msg = (data && (data.error?.message || data.error || data.message)) || ('HTTP ' + resp.status);
    throw new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
  }
  return data;
}

let toastTimer = null;
function toast(msg, type = 'success') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast toast-' + type + ' show';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), 3200);
}

// 视图加载失败的统一出口：会话过期（已跳登录页）保持静默，其余一律 toast——
// 静默吞掉 500 时面板会永远停在"加载中…"或陈旧数据上，看起来像空库
function reportLoadError(e) {
  const login = document.getElementById('loginPage');
  if (login && login.style.display !== 'none') return;
  toast(tf('加载失败: %s', e.message), 'error');
}

function openModal(html) {
  document.getElementById('modalBox').innerHTML = html;
  document.getElementById('modalOverlay').classList.add('show');
}
function closeModal() {
  document.getElementById('modalOverlay').classList.remove('show');
  // 关闭弹窗时清理 OAuth 轮询，避免遮罩关闭后定时器泄漏
  if (typeof cancelOAuth === 'function' && window._oauthState) cancelOAuth();
}

function esc(s) {
  if (s === null || s === undefined) return '';
  return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function fmtNum(n) {
  n = Number(n || 0);
  if (CURRENT_LANG === 'en') {
    if (n >= 1e9) return (Math.round(n / 1e8) / 10) + 'B';
    if (n >= 1e6) return (Math.round(n / 1e5) / 10) + 'M';
    if (n >= 1e4) return (Math.round(n / 1e3) / 10) + 'K';
  } else {
    if (n >= 1e8) return (Math.round(n / 1e7) / 10) + '亿';
    if (n >= 1e4) return (Math.round(n / 1e3) / 10) + '万';
  }
  return String(Math.round(n));
}

// pctText 比例（0~1）转百分比文案；后端未返回（无样本/旧数据）时显示 -
function pctText(v) {
  return (v === null || v === undefined || isNaN(v)) ? '-' : (v * 100).toFixed(1) + '%';
}

function fmtEpoch(sec) {
  if (!sec) return '-';
  const d = new Date(sec * 1000);
  return d.toLocaleString(currentLocale(), { hour12: false });
}

function fmtAgo(sec) {
  if (!sec) return t('从未');
  // 服务器时间可能领先本地时钟：负差值夹为 0，避免显示"-30秒前"
  const diff = Math.max(0, Math.floor(Date.now() / 1000) - sec);
  if (diff < 60) return tf('%s秒前', diff);
  if (diff < 3600) return tf('%s分钟前', Math.floor(diff / 60));
  if (diff < 86400) return tf('%s小时前', Math.floor(diff / 3600));
  return tf('%s天前', Math.floor(diff / 86400));
}

const STATUS_META = {
  active:    ['badge-success', '正常'],
  cooling:   ['badge-warning', '冷却中'],
  exhausted: ['badge-danger', '额度耗尽'],
  invalid:   ['badge-danger', '凭证失效'],
  inactive:  ['badge-info', '未激活'],
  disabled:  ['badge-secondary', '已禁用'],
};
function statusBadge(st) {
  const m = STATUS_META[st] || ['badge-secondary', esc(st || '-')];
  return `<span class="badge ${m[0]}">${t(m[1])}</span>`;
}

// ---- 登录 ----

function showLogin() {
  document.getElementById('loginPage').style.display = 'flex';
  document.getElementById('mainApp').style.display = 'none';
}
function showApp(username) {
  document.getElementById('loginPage').style.display = 'none';
  document.getElementById('mainApp').style.display = 'block';
  document.getElementById('welcomeUser').textContent = username || 'admin';
  document.getElementById('userAvatar').textContent = (username || 'A')[0].toUpperCase();
  loadAll();
}

async function doLogin() {
  const loginBtn = document.querySelector('#loginPage .btn-primary');
  if (loginBtn && loginBtn.disabled) return; // 在途请求防重（Enter 连击）
  const username = document.getElementById('loginUser').value.trim() || 'admin';
  const password = document.getElementById('loginPass').value;
  const errEl = document.getElementById('loginError');
  errEl.textContent = '';
  errEl.style.display = 'none';
  const btn = document.querySelector('#loginPage .btn-primary');
  if (btn) btn.disabled = true;
  try {
    const data = await api('/api/login', { method: 'POST', body: { username, password } });
    showApp(data.username);
    if (data.is_default_password) warnDefaultPassword();
  } catch (e) {
    const msg = e.message || t('登录失败');
    errEl.textContent = msg;
    errEl.style.display = 'block';   // 关键：显示错误框（CSS 默认 display:none）
    toast(msg, 'error');             // 同时弹 toast，确保可见
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function doLogout() {
  try { await api('/api/logout', { method: 'POST' }); } catch (e) { /* ignore */ }
  showLogin();
}

async function checkAuth() {
  try {
    const d = await api('/api/auth/check');
    showApp(d.username || 'admin');
    if (d.is_default_password) warnDefaultPassword();
  } catch (e) {
    showLogin();
  }
}

// warnDefaultPassword 初始随机口令未修改时的醒目提醒（后端 is_default_password=1）
function warnDefaultPassword() {
  if (warnDefaultPassword._shown) return;
  warnDefaultPassword._shown = true;
  setTimeout(() => toast(t('当前使用初始管理口令，请尽快在「设置 → 账号安全」中修改'), 'error'), 800);
}

document.getElementById('loginPass').addEventListener('keydown', e => { if (e.key === 'Enter') doLogin(); });

// ---- 导航 ----

document.querySelectorAll('.pill-nav .nav-link[data-section]').forEach(link => {
  link.addEventListener('click', () => switchSection(link.dataset.section));
});

function switchSection(name) {
  document.querySelectorAll('.pill-nav .nav-link').forEach(l => l.classList.toggle('active', l.dataset.section === name));
  document.querySelectorAll('.section').forEach(s => s.classList.toggle('active', s.id === 'section-' + name));
  if (name === 'dashboard') loadDashboard();
  if (name === 'accounts') { loadGroups(); loadAccounts(); }
  if (name === 'keys') loadKeys();
  if (name === 'activity') { loadPlans(); loadClaimRecords(); loadPlanRuns(); }
  if (name === 'usage') { loadUsageStats(); loadUsageRecords(); }
  if (name === 'llmtest') { loadLlmKey(); initLlmPlaceholders(); }
  if (name === 'settings') loadSettings();
}

function navMoreDo(btn, fn) {
  btn.closest('.nav-more').classList.remove('open');
  fn();
}

function switchSettingsTab(tab) {
  document.querySelectorAll('.sub-tab').forEach(el => el.classList.toggle('active', el.dataset.stab === tab));
  ['security', 'strategy', 'proxy', 'tls', 'captcha', 'models'].forEach(name => {
    const el = document.getElementById('stab-' + name);
    if (el) el.style.display = name === tab ? '' : 'none';
  });
}

// 切换语言后重载当前分区，让已渲染的动态内容跟随新语言（登录页无需重载）
// syncLlmPromptDefault 默认提示词是可编辑内容，属性型 i18n 覆盖不到：
// 仅当仍是任一语言的默认文案时跟随当前语言
function syncLlmPromptDefault() {
  const p = document.getElementById('llmPrompt');
  if (p && (p.value === '用一句话介绍你自己。' || p.value === 'Introduce yourself in one sentence.')) {
    p.value = t('用一句话介绍你自己。');
  }
}

function onLanguageChanged() {
  syncLlmPromptDefault();
  const main = document.getElementById('mainApp');
  if (!main || main.style.display === 'none') return;
  const active = document.querySelector('.section.active');
  if (active) switchSection(active.id.replace('section-', ''));
}

// ---- 仪表盘 ----

async function loadDashboard() {
  try {
    const d = await api('/api/dashboard');
    document.getElementById('statTotal').textContent = d.account_total ?? '-';
    document.getElementById('statSelectable').textContent = d.account_selectable ?? '-';
    document.getElementById('statRemaining').textContent = fmtNum(d.total_remaining);
    const u = d.usage_7d || {};
    document.getElementById('statRequests').textContent = fmtNum(u.requests);
    document.getElementById('statTokens').textContent = fmtNum(u.total_tokens);
    document.getElementById('statTtft').textContent = (u.avg_ttft_ms || 0) + 'ms';
    document.getElementById('statSuccess').textContent = pctText(u.success_rate);
    document.getElementById('statCacheHit').textContent = pctText(u.cache_hit_rate);
    document.getElementById('statTtftP').textContent = u.p50_ttft_ms != null
      ? (u.p50_ttft_ms || 0) + '/' + (u.p95_ttft_ms || 0) + 'ms' : '-';

    const cap = d.captcha || {};
    document.getElementById('dashStatus').innerHTML = `
      <div class="kv-grid">
        <div class="kv-item"><div class="k">${t('客户端版本伪装')}</div><div class="v">ZCode/${esc(d.app_version)}</div></div>
        <div class="kv-item"><div class="k">${t('验证码参数')}</div><div class="v">${cap.has_param ? (cap.fresh ? t('✅ 新鲜') : tf('⏳ 宽限期(%ss)', cap.param_age_s)) : t('— 未求解')}</div></div>
        <div class="kv-item"><div class="k">${t('验证码模式')}</div><div class="v">${cap.manual_mode ? t('有头手动') : t('无头自动')}</div></div>
        <div class="kv-item"><div class="k">${t('验证码配置')}</div><div class="v">${cap.config ? (cap.config.enabled ? tf('上游已启用 scene=%s', esc(cap.config.scene_id)) : t('上游未启用')) : t('未获取')}</div></div>
      </div>`;

    const sc = d.status_count || {};
    const gc = d.group_count || {};
    document.getElementById('dashAccounts').innerHTML = `
      <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:14px">
        ${Object.entries(sc).map(([k, v]) => statusBadge(k) + ' <b style="margin-right:10px">' + v + '</b>').join('')}
      </div>
      <div style="display:flex;gap:8px;flex-wrap:wrap">
        ${Object.entries(gc).map(([k, v]) => `<span class="pill-group">${esc(k)} · ${v}</span>`).join('')}
      </div>`;
  } catch (e) { reportLoadError(e); }
}

// ---- 分组 ----

let groupCache = [];
async function loadGroups() {
  try {
    const d = await api('/api/groups');
    groupCache = d.groups || [];
    const sel = document.getElementById('groupFilter');
    const cur = sel.value;
    sel.innerHTML = `<option value="">${t('全部分组')}</option>` + groupCache.map(g => `<option value="${esc(g)}">${esc(g)}</option>`).join('');
    sel.value = cur;
  } catch (e) { reportLoadError(e); }
}

function groupOptions(selected) {
  return `<option value="">${t('未分组')}</option>` + groupCache.map(g =>
    `<option value="${esc(g)}" ${g === selected ? 'selected' : ''}>${esc(g)}</option>`).join('') +
    `<option value="__new__">${t('＋ 新分组…')}</option>`;
}

// ---- 账号 ----

let accountsCache = [];

let loadAccountsSeq = 0;
async function loadAccounts() {
  const group = document.getElementById('groupFilter').value;
  const seq = ++loadAccountsSeq;
  try {
    const d = await api('/api/accounts' + (group ? '?group=' + encodeURIComponent(group) : ''));
    // 快速切换分组时旧响应可能后到：丢弃过期响应，避免 A 组数据渲染在 B 组筛选下
    if (seq !== loadAccountsSeq) return;
    accountsCache = d.accounts || [];
    renderAccounts();
  } catch (e) { if (seq === loadAccountsSeq) toast(e.message, 'error'); }
}

function quotaCell(a) {
  if (!a.plan_tier && !a.total_units) return `<span style="color:var(--c-text-lighter)">${t('未刷新')}</span>`;
  const pct = a.total_units > 0 ? Math.min(100, a.used_units / a.total_units * 100) : 0;
  return `<div class="quota-bar"><div class="progress-track" style="flex:1"><div class="progress-fill" style="width:${pct}%"></div></div>
    <span class="quota-num">${fmtNum(a.remaining)} / ${fmtNum(a.total_units)}</span></div>
    <div style="font-size:11px;color:var(--c-text-lighter);margin-top:3px">${esc(a.plan_tier || '')}${a.plan_expire ? ' · ' + tf('%s 到期', esc(a.plan_expire)) : ''}</div>`;
}

// showQuotaModal 点开额度 → 弹窗展示具体套餐与额度构成
function showQuotaModal(id) {
  const a = (accountsCache || []).find(x => x.id === id);
  if (!a) return;
  const q = a.quota;
  const fmtT = (s) => s ? new Date(s * 1000).toLocaleString(currentLocale(), { hour12: false }) : '-';
  const bar = (pct) => `<div class="progress-track" style="height:6px"><div class="progress-fill" style="width:${Math.min(100, pct || 0)}%"></div></div>`;
  let body;
  if (!q || (!q.plans || !q.plans.length) && (!q.items || !q.items.length)) {
    body = `<div class="empty"><p>${tf('暂无额度数据（%s）', esc(a.status))}</p></div>
      <div class="hint" style="margin-top:8px">${q && q.auth_failed ? t('凭证鉴权失败（401/403），请重新登录该账号。') : q && q.not_entitled ? t('该账号无 Coding Plan / 未激活。') : t('点击账号行「刷新」拉取额度后再查看。')}</div>`;
  } else {
    const slots = (q.plans && q.plans.length) ? q.plans : [{ plan_id: '-', name: q.plan_tier || t('套餐'), tier: q.plan_tier || '', status: a.status, expire: q.plan_expire, total: q.total, used: q.used, remaining: q.remaining, percent_used: q.percent_used, items: q.items || [] }];
    body = `
      <div class="kv-grid" style="margin-bottom:14px">
        <div class="kv-item"><div class="k">${t('套餐档位')}</div><div class="v">${esc(q.plan_tier || '-')}</div></div>
        <div class="kv-item"><div class="k">${t('到期时间')}</div><div class="v">${esc(q.plan_expire || '-')}</div></div>
        <div class="kv-item"><div class="k">${t('总 / 已用 / 剩余')}</div><div class="v">${fmtNum(q.total)} / ${fmtNum(q.used)} / ${fmtNum(q.remaining)}</div></div>
        <div class="kv-item"><div class="k">${t('使用占比')}</div><div class="v">${(q.percent_used || 0).toFixed(1)}%</div></div>
        <div class="kv-item"><div class="k">${t('数据源')}</div><div class="v">${esc(q.source || '-')}</div></div>
        <div class="kv-item"><div class="k">${t('刷新时间')}</div><div class="v">${fmtT(q.refreshed_at)}</div></div>
      </div>
      ${(q.channels && q.channels.length) ? `<div style="margin-bottom:14px">
        <div style="font-weight:700;font-size:13px;margin-bottom:6px">${t('双通道额度构成')}</div>
        ${q.channels.map(c => `
          <div style="display:flex;justify-content:space-between;align-items:center;gap:8px;border:1px solid var(--c-border);border-radius:8px;padding:8px 12px;margin-bottom:6px;font-size:12.5px">
            <div><b>${esc(c.source)}</b> <span class="badge badge-secondary">${esc(c.plan_tier || '-')}</span>
              ${c.exhausted ? `<span class="badge badge-danger">${t('已耗尽')}</span>` : `<span class="badge badge-success">${t('有余量')}</span>`}</div>
            <div>${tf('剩余 %s', fmtNum(c.remaining))}${c.next_reset ? ` · ${tf('重置 %s', fmtT(c.next_reset))}` : ''}</div>
          </div>`).join('')}
      </div>` : ''}
      ${slots.map(s => `
        <div style="border:1px solid var(--c-border);border-radius:10px;padding:12px 14px;margin-bottom:12px">
          <div style="display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:6px;margin-bottom:8px">
            <div style="font-weight:700">${esc(s.name || s.plan_id)} <span class="badge badge-purple">${esc(s.tier || '-')}</span> <span class="badge ${String(s.status).toLowerCase() === 'active' ? 'badge-success' : 'badge-secondary'}">${esc(s.status || '-')}</span></div>
            <div style="font-size:12px;color:var(--c-text-light)">${s.expire ? tf('到期 %s', esc(s.expire)) : ''}</div>
          </div>
          ${bar(s.percent_used)}
          <div style="font-size:12px;color:var(--c-text-light);margin:6px 0 8px">${tf('总 %s · 已用 %s · 剩余 %s（%s%）', fmtNum(s.total), fmtNum(s.used), fmtNum(s.remaining), (s.percent_used || 0).toFixed(1))}</div>
          ${(s.items && s.items.length) ? `<div class="table-wrap"><table>
            <thead><tr><th>${t('模型 / 权益')}</th><th>${t('总额')}</th><th>${t('已用')}</th><th>${t('剩余')}</th><th>${t('占比')}</th><th>${t('周期 / 到期')}</th></tr></thead>
            <tbody>${s.items.map(it => `<tr>
              <td>${esc(it.name)}</td>
              <td>${fmtNum(it.total)}</td>
              <td>${fmtNum(it.used)}</td>
              <td><b>${fmtNum(it.remaining)}</b></td>
              <td>${(it.percent_used || 0).toFixed(1)}%</td>
              <td style="font-size:11.5px;color:var(--c-text-light)">${esc(it.period_end || '-')}</td>
            </tr>`).join('')}</tbody></table></div>` : `<div class="hint">${t('该套餐无明细额度项')}</div>`}
        </div>`).join('')}
    `;
  }
  openModal(`<h3>${tf('套餐与额度构成 · %s', esc(a.display_name || a.email || ('#' + id)))}</h3>
    ${body}
    <div class="actions">
      <button class="btn btn-secondary" onclick="refreshQuota(${id});closeModal()">${t('刷新额度')}</button>
      <button class="btn btn-secondary" onclick="closeModal()">${t('关闭')}</button>
    </div>`);
}

function renderAccounts() {
  const el = document.getElementById('accountsTable');
  if (!accountsCache.length) {
    el.innerHTML = `<div class="empty"><p>${t('暂无账号，点击「导入本地账号」或「OAuth 登录」开始')}</p></div>`;
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr>
      <th>${t('账号')}</th><th>${t('分组')}</th><th>${t('认证')}</th><th>${t('状态')}</th><th>${t('套餐 / 剩余额度')}</th>
      <th>${t('设备指纹')}</th><th>${t('使用/失败')}</th><th>${t('最近活动')}</th><th style="width:200px">${t('操作')}</th>
    </tr></thead>
    <tbody>${accountsCache.map(a => `
      <tr>
        <td><div style="font-weight:700">${esc(a.display_name || a.email || a.user_id)}</div>
            <div style="font-size:11px;color:var(--c-text-lighter)">${esc(a.email || '')}</div>
            ${a.remark ? `<div style="font-size:11px;color:var(--c-text-lighter)">${tf('备注: %s', esc(a.remark))}</div>` : ''}</td>
        <td>${a.group ? `<span class="pill-group">${esc(a.group)}</span>` : '<span style="color:var(--c-text-lighter)">-</span>'}</td>
        <td><span class="badge ${a.auth_type === 'jwt' ? 'badge-info' : 'badge-purple'}">${a.auth_type === 'jwt' ? 'JWT' : 'API Key'}</span>
            ${a.has_api_key && a.auth_type === 'jwt' ? `<div style="font-size:10.5px;color:var(--c-text-lighter);margin-top:3px">${t('+APIKey回退')}${paidStateText(a)}</div>` : ''}</td>
        <td>${statusBadge(a.status)}${!a.enabled ? ` <span class="badge badge-secondary">${t('停用')}</span>` : ''}
            ${a.last_error ? `<div style="font-size:10.5px;color:var(--c-danger);margin-top:3px;max-width:160px;overflow:hidden;text-overflow:ellipsis" title="${esc(a.last_error)}">${esc(a.last_error)}</div>` : ''}
            ${a.paid_last_error ? `<div style="font-size:10.5px;color:var(--c-warning,var(--c-text-lighter));margin-top:2px;max-width:160px;overflow:hidden;text-overflow:ellipsis" title="${esc(a.paid_last_error)}">[${t('付费')}] ${esc(a.paid_last_error)}</div>` : ''}</td>
        <td style="min-width:190px;cursor:pointer" title="${t('点击查看套餐与额度构成')}" onclick="showQuotaModal(${a.id})">${quotaCell(a)}</td>
        <td><span class="mono" title="${esc(a.device_mid)}">${a.device_mid ? esc(a.device_mid.slice(0, 8)) + '…' : '-'}</span></td>
        <td>${a.use_count} / ${a.fail_count}<div style="font-size:10.5px;color:var(--c-text-lighter)">${fmtAgo(a.last_used_at)}</div></td>
        <td style="max-width:170px">${a.last_claim_at ? `<div style="font-size:11px">${esc(a.last_claim_plan || '')}</div><div style="font-size:10.5px;color:var(--c-text-lighter)">${esc((a.last_claim_msg || '').slice(0, 40))}</div>` : '<span style="color:var(--c-text-lighter)">-</span>'}</td>
        <td class="actions-cell">
          <button class="btn btn-sm btn-secondary" onclick="refreshQuota(${a.id})">${t('刷新')}</button>
          <button class="btn btn-sm btn-primary" onclick="claimNow(${a.id})">${t('领活动')}</button>
          <button class="btn btn-sm btn-secondary" onclick="showAccountActions(${a.id})">${t('更多 ▾')}</button>
        </td>
      </tr>`).join('')}
    </tbody></table></div>`;
}

// paidStateText 付费通道状态小字（双通道账号）：回退关闭 / 付费冷却中；正常时不显示
function paidStateText(a) {
  if (a.paid_fallback === false) return ' · ' + t('付费回退关');
  if (a.paid_cooling_until && a.paid_cooling_until > Date.now() / 1000) {
    return ' · ' + tf('付费冷却至 %s', new Date(a.paid_cooling_until * 1000).toLocaleTimeString());
  }
  return '';
}

// togglePaidFallback 开关单账号的付费通道回退（双通道账号：JWT 免费额度 + APIKey 按量计费）
async function togglePaidFallback(id, enable) {
  try {
    await api(`/api/accounts/${id}`, { method: 'PUT', body: { paid_fallback: enable } });
    toast(enable ? t('付费回退已开启') : t('付费回退已关闭'));
    closeModal();
    loadAccounts();
  } catch (e) { toast(tf('操作失败: %s', e.message), 'error'); }
}

// showAccountActions 用模态菜单承载账号操作（表格内下拉会被 overflow 容器裁剪）
function showAccountActions(id) {
  const a = (accountsCache || []).find(x => x.id === id);
  if (!a) return;
  openModal(`<h3>${tf('账号操作 · %s', esc(a.display_name || a.email || ('#' + id)))}</h3>
    <div style="display:grid;gap:8px">
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="detectNow(${id})">${t('🔍 检测活动')}</button>
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="activateNow(${id})">${t('⚡ 激活套餐')}</button>
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="resetQuota(${id})">${t('♻️ 配额重置（Coding Plan）')}</button>
      ${a.has_jwt && a.has_api_key ? `<button class="btn btn-secondary" style="justify-content:flex-start" onclick="togglePaidFallback(${id}, ${a.paid_fallback ? 'false' : 'true'})">${a.paid_fallback ? t('🚫 关闭付费回退') : t('✅ 开启付费回退')}</button>` : ''}
      <button class="btn btn-secondary" style="justify-content:flex-start" onclick="editAccount(${id})">${t('✏️ 编辑分组 / 备注')}</button>
      ${a.has_creds_snapshot || a.has_jwt ? `<button class="btn btn-secondary" style="justify-content:flex-start" onclick="switchBack(${id})">${t('💾 切回本地客户端')}</button>` : ''}
      ${a.has_creds_snapshot ? `<button class="btn btn-secondary" style="justify-content:flex-start" onclick="restoreLocal(${id})">${t('↩️ 从快照还原本地')}</button>` : ''}
      <button class="btn btn-danger" style="justify-content:flex-start" onclick="deleteAccount(${id})">${t('🗑 删除账号')}</button>
    </div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('关闭')}</button></div>`);
}

async function refreshQuota(id) {
  try {
    await api(`/api/accounts/${id}/refresh`, { method: 'POST' });
    toast(t('额度已刷新'));
    loadAccounts();
  } catch (e) { toast(tf('刷新失败: %s', e.message), 'error'); }
}

async function refreshAllQuota() {
  toast(t('正在刷新所有账号额度…'), 'info');
  try {
    // 始终取全量列表：accountsCache 可能带着分组筛选，只刷当前组却提示"全部"会误导
    const list = (await api('/api/accounts')).accounts || [];
    for (const a of list) {
      try { await api(`/api/accounts/${a.id}/refresh`, { method: 'POST' }); } catch (e) { /* 单个失败继续 */ }
    }
    toast(t('全部刷新完成'));
  } catch (e) { toast(tf('刷新失败: %s', e.message), 'error'); }
  loadAccounts(); loadDashboard();
}

async function claimNow(id) {
  toast(t('正在检测并领取活动（含验证码求解，约 10-30 秒）…'), 'info');
  try {
    const r = await api(`/api/accounts/${id}/claim`, { method: 'POST' });
    if (r.ok) toast(tf('领取成功: %s', r.plan_name || ''));
    else toast(tf('领取未成功: %s', r.message), 'error');
    loadAccounts();
  } catch (e) { toast(tf('领取失败: %s', e.message), 'error'); }
}

async function detectNow(id) {
  closeModal();
  try {
    const r = await api(`/api/accounts/${id}/detect`);
    const plans = r.plans || [];
    openModal(`<h3>${t('活动检测')}</h3>
      ${plans.length ? plans.map(p => `
        <div style="border:1px solid var(--c-border);border-radius:10px;padding:12px 14px;margin-bottom:10px">
          <div style="font-weight:700;margin-bottom:4px">${esc(p.name)} <span class="badge badge-purple">${tf('优先级 %s', p.priority)}</span></div>
          <div style="font-size:12px;color:var(--c-text-light);margin-bottom:6px">${esc(p.description || '')}</div>
          ${(p.grants || []).map(g => `<div style="font-size:12.5px">🎁 ${esc(g)}</div>`).join('')}
          <div class="mono" style="color:var(--c-text-lighter);margin-top:6px">${esc(p.plan_id)}</div>
        </div>`).join('') : `<div class="empty"><p>${t('当前无可领取活动')}</p></div>`}
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('关闭')}</button>
      ${plans.length ? `<button class="btn btn-primary" onclick="claimNow(${id})">${t('立即领取最高优先级')}</button>` : ''}</div>`);
  } catch (e) { toast(tf('检测失败: %s', e.message), 'error'); }
}

async function detectAllAccounts() {
  let list;
  try {
    // 全量列表：不受当前分组筛选影响（与"检测全部"语义一致）
    list = (await api('/api/accounts')).accounts || [];
  } catch (e) { toast(tf('检测失败: %s', e.message), 'error'); return; }
  toast(tf('正在检测 %s 个账号的活动…', list.length), 'info');
  let found = 0;
  for (const a of list) {
    try {
      const r = await api(`/api/accounts/${a.id}/detect`);
      if ((r.plans || []).length) found++;
    } catch (e) { /* continue */ }
  }
  toast(tf('检测完成：%s/%s 个账号发现活动', found, list.length));
  loadClaimRecords();
}

async function resetQuota(id) {
  closeModal();
  toast(t('正在查询重置机会…'), 'info');
  try {
    const st = await api(`/api/accounts/${id}/reset-status`);
    if (!st.ok) {
      openModal(`<h3>${t('配额重置')}</h3><div class="empty"><p>${esc(st.message || t('不可用'))}</p></div>
        <div class="hint" style="margin-top:10px">${t('该接口仅付费 Coding Plan 账号可用（Start Plan 返回 3101 coding plan is required）。')}</div>
        <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('关闭')}</button></div>`);
      return;
    }
    const s = st.status;
    const five = (s.available_five_hour_resets || []).length;
    const week = (s.available_week_resets || []).length;
    openModal(`<h3>${t('配额重置机会')}</h3>
      <div class="kv-grid" style="margin-bottom:14px">
        <div class="kv-item"><div class="k">${t('5 小时窗口重置')}</div><div class="v">${tf('%s 次可用', five)}</div></div>
        <div class="kv-item"><div class="k">${t('周重置')}</div><div class="v">${tf('%s 次可用', week)}</div></div>
        <div class="kv-item"><div class="k">${t('最近 5h 重置')}</div><div class="v">${s.latest_five_hour_reset_history ? fmtEpoch(s.latest_five_hour_reset_history.used_at / 1000) : t('无')}</div></div>
        <div class="kv-item"><div class="k">${t('最近周重置')}</div><div class="v">${s.latest_week_reset_history ? fmtEpoch(s.latest_week_reset_history.used_at / 1000) : t('无')}</div></div>
      </div>
      <div class="hint">${t('消耗一次机会立即恢复对应窗口配额（优先 five_hour）。')}</div>
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
      <button class="btn btn-warning" onclick="doReset(${id})" ${five + week === 0 ? 'disabled' : ''}>${t('执行重置')}</button></div>`);
  } catch (e) { toast(e.message, 'error'); }
}

async function doReset(id) {
  closeModal();
  toast(t('正在执行配额重置…'), 'info');
  try {
    const r = await api(`/api/accounts/${id}/reset`, { method: 'POST' });
    if (r.ok) toast(r.message || t('重置成功'));
    else toast(tf('重置失败: %s', r.message), 'error');
    loadAccounts();
  } catch (e) { toast(e.message, 'error'); }
}

async function activateNow(id) {
  closeModal();
  toast(t('正在上报激活事件…'), 'info');
  try {
    const r = await api(`/api/accounts/${id}/activate`, { method: 'POST' });
    if (r.ok) toast(tf('激活成功: %s', r.message || ''));
    else toast(tf('激活未完成: %s', r.message), 'error');
    loadAccounts();
  } catch (e) { toast(tf('激活失败: %s', e.message), 'error'); }
}

function editAccount(id) {
  closeModal();
  const a = accountsCache.find(x => x.id === id);
  if (!a) return;
  openModal(`<h3>${t('编辑账号')}</h3>
    <div class="form-group"><label>${t('分组')}</label><select id="editGroup">${groupOptions(a.group)}</select></div>
    <div class="form-group"><label>${t('备注')}</label><input type="text" id="editRemark" value="${esc(a.remark)}"></div>
    <div class="form-group"><label>${t('Priority（priority 策略：数值小者先用，促销层默认 50）')}</label><input type="number" id="editPriority" class="form-input" min="1" max="9999" value="${a.priority || 100}"></div>
    <div class="form-group"><label><input type="checkbox" id="editEnabled" ${a.enabled ? 'checked' : ''} style="width:auto;margin-right:6px">${t('启用（参与轮询）')}</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="submitEditAccount(${id})">${t('保存')}</button></div>`);
}

async function submitEditAccount(id) {
  let group = document.getElementById('editGroup').value;
  if (group === '__new__') {
    group = prompt(t('新分组名称:'));
    if (!group) return;
  }
  try {
    // 空字段回落默认 100；显式键入的非法值如实 400（静默改写成 100 会让
    // 管理员以为 0 生效了）
    const pv = parseInt(document.getElementById('editPriority').value, 10);
    const priority = Number.isNaN(pv) ? 100 : pv;
    await api(`/api/accounts/${id}`, { method: 'PUT', body: {
      group, remark: document.getElementById('editRemark').value,
      enabled: document.getElementById('editEnabled').checked,
      priority,
    }});
    closeModal(); toast(t('已保存')); loadGroups(); loadAccounts();
  } catch (e) { toast(e.message, 'error'); }
}

async function deleteAccount(id) {
  closeModal();
  const a = accountsCache.find(x => x.id === id);
  if (!confirm(tf('确认删除账号 %s？此操作不可恢复。', a ? (a.email || a.display_name) : id))) return;
  try {
    await api(`/api/accounts/${id}`, { method: 'DELETE' });
    toast(t('已删除')); loadAccounts(); loadDashboard();
  } catch (e) { toast(e.message, 'error'); }
}

async function switchBack(id) {
  closeModal();
  const a = accountsCache.find(x => x.id === id);
  openModal(`<h3>${t('一键切回本地客户端')}</h3>
    <p style="font-size:13px;color:var(--c-text-light);margin-bottom:14px">
    ${tf('将把账号 <b>%s</b> 的凭证重新加密写回本机<span class="mono">~/.zcode/v2/credentials.json</span> 与 <span class="mono">config.json</span>（原文件自动备份到 data/backups/）。', esc(a ? a.email || a.display_name : id))}</p>
    <div class="form-group"><label><input type="checkbox" id="killClient" style="width:auto;margin-right:6px">${t('写回后结束 ZCode.exe 进程（下次启动生效）')}</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-warning" onclick="doSwitchBack(${id})">${t('确认切回')}</button></div>`);
}

async function doSwitchBack(id) {
  const kill = document.getElementById('killClient').checked;
  try {
    const r = await api(`/api/accounts/${id}/switch-back`, { method: 'POST', body: { kill_client: kill } });
    closeModal(); toast(r.message || t('已切回本地客户端'));
  } catch (e) { toast(tf('切回失败: %s', e.message), 'error'); }
}

async function restoreLocal(id) {
  closeModal();
  if (!confirm(t('用该账号导入时的快照覆盖本地客户端当前登录态？'))) return;
  try {
    const r = await api(`/api/accounts/${id}/restore-local`, { method: 'POST' });
    toast(r.message || t('已还原'));
  } catch (e) { toast(tf('还原失败: %s', e.message), 'error'); }
}

// ---- 账号导入 ----

async function importLocalAccount() {
  toast(t('正在从本地 ZCode 客户端导入…'), 'info');
  try {
    const group = document.getElementById('groupFilter')?.value || '';
    const r = await api('/api/accounts/import/local', { method: 'POST', body: { group } });
    toast(tf('导入成功: %s', r.account.email || r.account.display_name));
    switchSection('accounts');
  } catch (e) { toast(tf('导入失败: %s', e.message), 'error'); }
}

function showPasteModal() {
  openModal(`<h3>${t('粘贴导入')}</h3>
    <div class="form-group"><label>${t('通道')}</label>
      <select id="pasteProvider"><option value="zai">Z.AI (zcode.z.ai)</option><option value="bigmodel">BigModel (open.bigmodel.cn)</option></select></div>
    <div class="form-group"><label>${t('名称（可选）')}</label><input type="text" id="pasteName" placeholder="${t('账号备注名')}"></div>
    <div class="form-group"><label>${t('凭证（JWT 或 API Key）')}</label>
      <textarea id="pasteSecret" placeholder="${t('eyJhbGci… 三段 JWT，或 xxx.yyy 格式 API Key')}"></textarea></div>
    <div class="form-group"><label>${t('分组')}</label><select id="pasteGroup">${groupOptions('')}</select></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="submitPaste()">${t('导入')}</button></div>`);
}

async function submitPaste() {
  let group = document.getElementById('pasteGroup').value;
  if (group === '__new__') { group = prompt(t('新分组名称:')); if (!group) return; }
  try {
    const r = await api('/api/accounts/import/paste', { method: 'POST', body: {
      provider: document.getElementById('pasteProvider').value,
      name: document.getElementById('pasteName').value,
      secret: document.getElementById('pasteSecret').value.trim(),
      group,
    }});
    closeModal(); toast(t('导入成功')); switchSection('accounts');
  } catch (e) { toast(tf('导入失败: %s', e.message), 'error'); }
}

// ---- OAuth 登录 ----

let oauthPollTimer = null;
let oauthRedirectTimer = null;

function showOAuthModal() {
  openModal(`<h3>${t('OAuth 登录新账号')}</h3>
    <div class="form-group"><label>${t('分组')}</label><select id="oauthGroup">${groupOptions('')}</select></div>
    <div class="form-group"><label>${t('方式')}</label>
      <select id="oauthMode">
        <option value="manual">${t('手动粘贴（推荐：Z.AI 仅注册了 zcode.z.ai/login 回跳）')}</option>
        <option value="auto">${t('环回自动（实验：当前会报 Redirect URI not registered）')}</option>
      </select></div>
    <div id="oauthStep2"></div>
    <div class="actions"><button class="btn btn-secondary" onclick="cancelOAuth()">${t('取消')}</button>
    <button class="btn btn-primary" id="oauthStartBtn" onclick="startOAuth()">${t('开始登录')}</button></div>`);
}

async function startOAuth() {
  let group = document.getElementById('oauthGroup').value;
  if (group === '__new__') { group = prompt(t('新分组名称:')); if (!group) return; }
  const manual = document.getElementById('oauthMode').value === 'manual';
  const btn = document.getElementById('oauthStartBtn');
  btn.disabled = true;
  try {
    const r = await api('/api/accounts/oauth/start', { method: 'POST', body: { manual, group } });
    window._oauthState = r.state;
    if (!manual) {
      window.open(r.authorize_url, '_blank', 'noopener'); // noopener 下返回值恒为 null，不据此判断是否被拦截
      document.getElementById('oauthStep2').innerHTML =
        `<div class="hint" style="margin-top:10px">${t('已在新标签页打开 Z.AI 授权页，登录并授权后自动跳回本网关完成入库。')} <a href="${esc(r.authorize_url)}" target="_blank" rel="noopener">${t('打开授权页')}</a></div>
         <div class="hint" style="margin-top:6px;color:var(--c-warning-dark)">${t('若授权后无法自动跳回（当前上游常报 Redirect URI 未注册），请点「取消」，把模式切换为「手动粘贴」后重新发起登录并提交新授权页跳转的完整 URL——旧授权码不可复用。')}</div>
         <div id="oauthStatus" style="margin-top:8px;font-size:13px"></div>`;
      btn.disabled = false; // 允许改手动模式后重新发起（旧 code 因 redirect_uri 不同无法复用）
      pollOAuth();
    } else {
      window.open(r.authorize_url, '_blank', 'noopener');
      document.getElementById('oauthStep2').innerHTML =
        `<div class="hint" style="margin-top:10px">${t('已在新标签页打开 Z.AI 授权页。步骤：① 登录并同意授权 → ② 浏览器会跳到 <b>zcode.z.ai/login?code=…</b> → ③ 复制该地址栏<b>完整 URL</b>粘贴到下面 → ④ 提交兑换。')} <a href="${esc(r.authorize_url)}" target="_blank" rel="noopener">${t('打开授权页')}</a></div>
         <div class="form-group" style="margin-top:10px"><label>${t('粘贴回跳 URL（或仅 code）')}</label>
         <textarea id="oauthManualInput" class="form-textarea" style="min-height:70px" placeholder="https://zcode.z.ai/login?code=...&state=..."></textarea></div>
         <button class="btn btn-success" onclick="submitOAuthManual()">${t('提交兑换')}</button>
         <div id="oauthStatus" style="margin-top:8px;font-size:13px"></div>`;
    }
  } catch (e) {
    btn.disabled = false;
    toast(tf('发起登录失败: %s', e.message), 'error');
  }
}

async function submitOAuthManual() {
  const input = document.getElementById('oauthManualInput').value.trim();
  if (!input) return toast(t('请粘贴回跳 URL 或 code'), 'error');
  const st = document.getElementById('oauthStatus');
  st.textContent = t('兑换中…');
  try {
    await api('/api/accounts/oauth/manual', { method: 'POST', body: { state: window._oauthState, input } });
    st.textContent = '';
    toast(t('登录成功，账号已入库'));
    cancelOAuth();
    switchSection('accounts');
  } catch (e) { st.textContent = ''; toast(e.message, 'error'); }
}

function pollOAuth() {
  clearInterval(oauthPollTimer);
  oauthPollTimer = setInterval(async () => {
    if (!window._oauthState) return;
    try {
      const f = await api('/api/accounts/oauth/status?state=' + encodeURIComponent(window._oauthState));
      if (!window._oauthState) return; // await 期间用户已取消：不得再拉起跳转定时器
      const el = document.getElementById('oauthStatus');
      if (f.status === 'ready') {
        clearInterval(oauthPollTimer);
        toast(tf('登录成功: %s', f.email || ''));
        oauthRedirectTimer = setTimeout(() => { closeModal(); switchSection('accounts'); }, 800);
      } else if (f.status === 'failed') {
        clearInterval(oauthPollTimer);
        if (el) el.innerHTML = `<span style="color:var(--c-danger)">${esc(f.message)}</span>`;
        document.getElementById('oauthStartBtn').disabled = false;
      } else if (el && f.status === 'exchanging') {
        el.textContent = t('正在兑换 token 并提取 API Key…');
      }
    } catch (e) {
      // 流程过期/会话失效：终止轮询，否则弹窗叠在登录页上 1.5s 一次打 401；
      // 同时让死状态可见（授权可能在网关侧重已完成，轮询却静默死了）
      clearInterval(oauthPollTimer);
      const sel = document.getElementById('oauthStatus');
      if (sel) sel.textContent = tf('轮询已停止: %s（如已完成授权请刷新账号列表）', e.message);
    }
  }, 1500);
}

function cancelOAuth() {
  clearInterval(oauthPollTimer);
  clearTimeout(oauthRedirectTimer);
  window._oauthState = null;
  closeModal();
}

// ---- 网关 Key ----

let keysCache = [];

async function loadKeys() {
  try {
    const d = await api('/api/keys');
    keysCache = d.keys || [];
    renderKeys();
  } catch (e) { toast(e.message, 'error'); }
}

// keyQuotaCell 配额列：quota_total=0 不限（不画进度条），否则 已用/总量 + 进度条
function keyQuotaCell(k) {
  if (!k.quota_total) return `<span style="color:var(--c-text-lighter)">${t('不限')}</span>`;
  const pct = Math.min(100, (k.quota_used || 0) / k.quota_total * 100);
  return `<div class="quota-bar"><div class="progress-track" style="flex:1"><div class="progress-fill" style="width:${pct}%"></div></div>
    <span class="quota-num">${fmtNum(k.quota_used)} / ${fmtNum(k.quota_total)}</span></div>`;
}

function renderKeys() {
  const el = document.getElementById('keysTable');
  if (!el) return;
  if (!keysCache.length) {
    el.innerHTML = `<div class="empty"><p>${t('暂无网关 Key，点击「+ 新建 Key」创建')}</p></div>`;
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr><th>${t('名称')}</th><th>${t('Key 前缀')}</th><th>${t('状态')}</th><th>RPM</th><th>${t('配额')}</th><th>${t('模型')}</th><th>${t('最近使用')}</th><th style="width:190px">${t('操作')}</th></tr></thead>
    <tbody>${keysCache.map(k => `
      <tr>
        <td style="font-weight:700">${esc(k.name)}</td>
        <td class="mono">${esc(k.key_prefix || '-')}</td>
        <td>${k.enabled ? `<span class="badge badge-success">${t('启用')}</span>` : `<span class="badge badge-secondary">${t('停用')}</span>`}</td>
        <td>${k.rpm_limit ? k.rpm_limit : `<span style="color:var(--c-text-lighter)">${t('不限')}</span>`}</td>
        <td style="min-width:150px">${keyQuotaCell(k)}</td>
        <td style="max-width:160px" title="${esc(k.models || '')}">${k.models ? `<span class="mono">${esc(k.models)}</span>` : `<span style="color:var(--c-text-lighter)">${t('全部')}</span>`}</td>
        <td style="font-size:12px">${fmtAgo(k.last_used_at)}</td>
        <td class="actions-cell">
          <button class="btn btn-sm ${k.enabled ? 'btn-secondary' : 'btn-primary'}" onclick="toggleKey(${k.id})">${k.enabled ? t('停用') : t('启用')}</button>
          <button class="btn btn-sm btn-secondary" onclick="showKeyModal(${k.id})">${t('编辑')}</button>
          <button class="btn btn-sm btn-danger" onclick="deleteKey(${k.id})">${t('删除')}</button>
        </td>
      </tr>`).join('')}</tbody></table></div>`;
}

function showKeyModal(id) {
  const k = keysCache.find(x => x.id === id) || {};
  openModal(`<h3>${id ? t('编辑网关 Key') : t('新建网关 Key')}</h3>
    <div class="form-group"><label>${t('名称')}</label><input type="text" id="keyName" value="${esc(k.name || '')}" placeholder="${t('例如：ci-机器人')}"></div>
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
      <div class="form-group"><label>${t('RPM 限制（0=不限）')}</label><input type="number" id="keyRpm" class="form-input" min="0" max="100000" value="${k.rpm_limit ?? ''}" placeholder="${t('0=不限')}"></div>
      <div class="form-group"><label>${t('总配额（tokens，0=不限）')}</label><input type="number" id="keyQuota" class="form-input" min="0" value="${k.quota_total ?? ''}" placeholder="${t('0=不限')}"></div>
    </div>
    <div class="form-group"><label>${t('模型白名单（逗号分隔，留空=全部）')}</label><input type="text" id="keyModels" class="form-input mono" value="${esc(k.models || '')}" placeholder="${t('glm-5.3,glm-5.2 留空=全部')}"></div>
    ${id ? '' : `<div class="form-group"><label>${t('管理员密码（创建需口令验证）')}</label><input type="password" id="keyVerifyPwd" class="form-input" autocomplete="off"></div>`}
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="saveKey(${id || 0})">${t('保存')}</button></div>`);
}

async function saveKey(id) {
  const name = document.getElementById('keyName').value.trim();
  if (!name) return toast(t('请填写名称'), 'error');
  const body = {
    name,
    rpm_limit: Number(document.getElementById('keyRpm').value || 0),
    quota_total: Number(document.getElementById('keyQuota').value || 0),
    models: document.getElementById('keyModels').value.trim(),
  };
  if (!id) {
    // 创建即回明文：服务端要求口令步进（ stolen session 不得铸无限制 Key）
    body.verify_password = document.getElementById('keyVerifyPwd').value;
    if (!body.verify_password) return toast(t('需要管理员密码'), 'error');
  }
  try {
    if (id) {
      await api('/api/keys/' + id, { method: 'PUT', body });
      closeModal(); toast(t('已保存'));
    } else {
      const r = await api('/api/keys', { method: 'POST', body });
      showCreatedKey((r && r.key) || {});
    }
    loadKeys();
  } catch (e) { toast(e.message, 'error'); }
}

// showCreatedKey 明文 Key 仅创建响应返回一次：模态突出展示 + 复制按钮 + 不再显示警告
function showCreatedKey(k) {
  openModal(`<h3>${t('网关 Key 已创建')}</h3>
    <div class="warn-box">${t('请立即保存明文 Key，关闭后不再显示！')}</div>
    <div style="display:flex;gap:8px;align-items:center;margin-bottom:12px">
      <code id="newKeyPlain" style="flex:1;padding:10px 12px;font-size:13px;word-break:break-all;white-space:normal">${esc(k.key || '')}</code>
      <button class="btn btn-secondary" onclick="copyToClipboard(document.getElementById('newKeyPlain').textContent)">${t('复制')}</button>
    </div>
    <div class="hint">${t('Key 前缀')}: <span class="mono">${esc(k.key_prefix || '-')}</span></div>
    <div class="actions"><button class="btn btn-primary" onclick="closeModal()">${t('我已保存，关闭')}</button></div>`);
}

async function toggleKey(id) {
  const k = keysCache.find(x => x.id === id);
  if (!k) return;
  try {
    await api('/api/keys/' + id, { method: 'PUT', body: { enabled: !k.enabled } });
    toast(k.enabled ? t('已停用') : t('已启用'));
    loadKeys();
  } catch (e) { toast(e.message, 'error'); }
}

async function deleteKey(id) {
  const k = keysCache.find(x => x.id === id);
  if (!confirm(tf('确认删除网关 Key %s？此操作不可恢复。', k ? k.name : '#' + id))) return;
  try { await api('/api/keys/' + id, { method: 'DELETE' }); toast(t('已删除')); loadKeys(); }
  catch (e) { toast(e.message, 'error'); }
}

// ---- 活动计划 ----

let plansCache = [];

async function loadPlans() {
  try {
    const d = await api('/api/plans');
    plansCache = d.plans || [];
    renderPlans();
  } catch (e) { toast(e.message, 'error'); }
}

const TASK_LABEL = { detect: '检测活动', claim: '一键领取', activate: '激活套餐', reset: '配额重置' };
function taskLabel(type) { return t(TASK_LABEL[type] || type); }

function renderPlans() {
  const el = document.getElementById('plansTable');
  if (!plansCache.length) {
    el.innerHTML = `<div class="empty"><p>${t('暂无计划，点击「+ 新建计划」创建')}</p></div>`;
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr><th>${t('计划名')}</th><th>${t('任务')}</th><th>cron</th><th>${t('目标')}</th><th>${t('间隔')}</th><th>${t('下次运行')}</th><th>${t('最近运行')}</th><th>${t('状态')}</th><th style="width:190px">${t('操作')}</th></tr></thead>
    <tbody>${plansCache.map(p => `
      <tr>
        <td style="font-weight:700">${esc(p.plan_name)}</td>
        <td><span class="badge badge-info">${esc(taskLabel(p.task_type))}</span></td>
        <td class="mono">${esc(p.cron_expr)}</td>
        <td>${p.target_type === 'single_account' ? tf('账号#%s', p.account_id) : p.target_type === 'group' ? tf('分组: %s', esc(p.account_group)) : t('全部账号')}</td>
        <td>${p.delay_seconds}s</td>
        <td style="font-size:12px">${p.task_type === 'reset' ? '-' : esc(p.next_run_at || '-')}</td>
        <td style="font-size:12px">${esc(p.last_run_at || '-')}<div style="color:var(--c-text-lighter);font-size:11px;max-width:200px;overflow:hidden;text-overflow:ellipsis" title="${esc(p.last_run_msg || '')}">${esc(p.last_run_msg || '')}</div></td>
        <td>${p.task_type === 'reset' ? `<span class="badge badge-warning">${t('重置仅支持手动执行，请删除此计划')}</span>` : p.is_active ? `<span class="badge badge-success">${t('启用')}</span>` : `<span class="badge badge-secondary">${t('停用')}</span>`}
            ${p.last_run_status ? `<div style="margin-top:3px">${p.last_run_status === 'success' ? '✅' : '❌'}</div>` : ''}</td>
        <td class="actions-cell">
          <button class="btn btn-sm btn-primary" onclick="runPlan(${p.id})" ${p.task_type === 'reset' ? 'disabled' : ''}>${t('立即运行')}</button>
          <button class="btn btn-sm btn-secondary" onclick="showPlanModal(${p.id})" ${p.task_type === 'reset' ? 'disabled' : ''}>${t('编辑')}</button>
          <button class="btn btn-sm btn-danger" onclick="deletePlan(${p.id})">${t('删除')}</button>
        </td>
      </tr>`).join('')}</tbody></table></div>`;
}

async function showPlanModal(id) {
  const p = plansCache.find(x => x.id === id) || {};
  if (p.task_type === 'reset') { toast(t('重置仅支持手动执行，请删除此计划'), 'error'); return; }
  // 账号缓存为空（如启动后直接进入活动页）、或缓存带着分组筛选而当前计划的
  // 目标账号不在其中时，拉取全量列表：否则 single_account 计划编辑时看不到
  // 真实目标，保存也会被拒
  let listLoadFailed = false;
  const targetMissing = id && p.target_type === 'single_account' &&
    !(accountsCache || []).some(a => a.id === p.account_id);
  if (!(accountsCache || []).length || targetMissing) {
    try { accountsCache = (await api('/api/accounts')).accounts || []; }
    catch (e) { listLoadFailed = true; toast(tf('账号列表加载失败: %s', e.message), 'error'); }
  }
  let accountOpts = (accountsCache.length ? accountsCache : []).map(a =>
    `<option value="${a.id}" ${p.account_id === a.id ? 'selected' : ''}>${esc(a.email || a.display_name || ('#' + a.id))}</option>`).join('');
  // 目标账号不在列表：列表加载失败或账号已被删除——如实区分，避免误导排查
  if (id && p.target_type === 'single_account' && p.account_id &&
      !accountsCache.some(a => a.id === p.account_id)) {
    const label = listLoadFailed ? t('当前目标，列表加载失败') : t('当前目标（账号已删除）');
    accountOpts += `<option value="${p.account_id}" selected>#${p.account_id}（${esc(label)}）</option>`;
  }
  openModal(`<h3>${id ? t('编辑计划') : t('新建计划')}</h3>
    <div class="form-group"><label>${t('计划名称')}</label><input type="text" id="planName" value="${esc(p.plan_name || '')}" placeholder="${t('例如：每日领取活动')}"></div>
    <div class="form-group"><label>${t('任务类型')}</label>
      <select id="planTask">
        <option value="claim" ${p.task_type === 'claim' ? 'selected' : ''}>${t('一键领取（检测+验证码+领取）')}</option>
        <option value="detect" ${p.task_type === 'detect' ? 'selected' : ''}>${t('仅检测活动')}</option>
        <option value="activate" ${p.task_type === 'activate' ? 'selected' : ''}>${t('激活套餐（上报激活事件）')}</option>
      </select></div>
    <div class="form-group"><label>${t('cron 表达式（分 时 日 月 周）')}</label>
      <input type="text" id="planCron" class="mono" value="${esc(p.cron_expr || '0 9 * * *')}" placeholder="0 9 * * *">
      <div class="hint">${t('示例：0 9 * * * = 每天 09:00；*/30 * * * * = 每 30 分钟')}</div></div>
    <div class="form-group"><label>${t('目标')}</label>
      <select id="planTarget" onchange="planTargetChange()">
        <option value="all_accounts" ${p.target_type === 'all_accounts' || !p.target_type ? 'selected' : ''}>${t('全部可用账号')}</option>
        <option value="group" ${p.target_type === 'group' ? 'selected' : ''}>${t('指定分组')}</option>
        <option value="single_account" ${p.target_type === 'single_account' ? 'selected' : ''}>${t('单个账号')}</option>
      </select></div>
    <div class="form-group" id="planGroupWrap" style="display:none"><label>${t('分组')}</label><select id="planGroup">${groupOptions(p.account_group || '')}</select></div>
    <div class="form-group" id="planAccountWrap" style="display:none"><label>${t('账号')}</label><select id="planAccount"><option value="0">${t('选择账号')}</option>${accountOpts}</select></div>
    <div class="form-group"><label>${t('账号间隔（秒，防风控，含 0~50% 随机抖动）')}</label>
      <input type="number" id="planDelay" value="${p.delay_seconds ?? 30}" min="0" max="3600"></div>
    <div class="form-group"><label><input type="checkbox" id="planActive" ${p.is_active !== false ? 'checked' : ''} style="width:auto;margin-right:6px">${t('启用')}</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="savePlan(${id || 0})">${t('保存')}</button></div>`);
  planTargetChange();
}

function planTargetChange() {
  const target = document.getElementById('planTarget').value;
  document.getElementById('planGroupWrap').style.display = target === 'group' ? '' : 'none';
  document.getElementById('planAccountWrap').style.display = target === 'single_account' ? '' : 'none';
}

async function savePlan(id) {
  let group = '';
  const gw = document.getElementById('planGroupWrap');
  if (gw.style.display !== 'none') {
    group = document.getElementById('planGroup').value;
    if (group === '__new__') { group = prompt(t('新分组名称:')); if (!group) return; }
  }
  const accountId = Number(document.getElementById('planAccount')?.value || 0);
  if (document.getElementById('planTarget').value === 'single_account' && !accountId) {
    // 拒绝把 single_account 计划的目标静默改成 0
    toast(t('请选择目标账号'), 'error');
    return;
  }
  const body = {
    plan_name: document.getElementById('planName').value.trim() || t('未命名计划'),
    task_type: document.getElementById('planTask').value,
    cron_expr: document.getElementById('planCron').value.trim(),
    target_type: document.getElementById('planTarget').value,
    account_group: group,
    account_id: accountId,
    delay_seconds: Number(document.getElementById('planDelay').value || 0),
    is_active: document.getElementById('planActive').checked,
    auto_pick: true,
  };
  try {
    if (id) await api('/api/plans/' + id, { method: 'PUT', body });
    else await api('/api/plans', { method: 'POST', body });
    closeModal(); toast(t('计划已保存')); loadPlans();
  } catch (e) { toast(e.message, 'error'); }
}

async function deletePlan(id) {
  if (!confirm(t('确认删除该计划？'))) return;
  try { await api('/api/plans/' + id, { method: 'DELETE' }); toast(t('已删除')); loadPlans(); }
  catch (e) { toast(e.message, 'error'); }
}

async function runPlan(id) {
  if (plansCache.find(p => p.id === id)?.task_type === 'reset') { toast(t('重置仅支持手动执行，请删除此计划'), 'error'); return; }
  try { await api(`/api/plans/${id}/run`, { method: 'POST' }); toast(t('已开始执行，见顶部进度'), 'info'); }
  catch (e) { toast(e.message, 'error'); }
}

async function pollRunning() {
  try {
    const d = await api('/api/plans/running');
    const bar = document.getElementById('planRunningBar');
    if (!bar) return;
    const running = d.running || [];
    if (!running.length) { bar.innerHTML = ''; return; }
    bar.innerHTML = running.map(s => `
      <div class="running-bar">
        <span class="rb-title">${tf('▶ %s（%s）', esc(s.plan_name), esc(taskLabel(s.task_type)))}</span>
        <div class="progress-track" style="flex:1;min-width:120px"><div class="progress-fill" style="width:${s.total ? s.done / s.total * 100 : 0}%"></div></div>
        <span class="rb-meta">${s.done}/${s.total} · ✅${s.success} ❌${s.fail}${s.current_account ? ' · ' + esc(s.current_account) : ''}</span>
      </div>`).join('');
  } catch (e) { /* 5s 轮询：持续失败不占 toast（会盖掉用户操作反馈） */ }
}

async function loadClaimRecords() {
  try {
    const d = await api('/api/claim-records?limit=60');
    const el = document.getElementById('claimRecordsTable');
    const recs = d.records || [];
    if (!recs.length) { el.innerHTML = `<div class="empty"><p>${t('暂无领取记录')}</p></div>`; return; }
    el.innerHTML = `<div class="table-wrap"><table>
      <thead><tr><th>${t('时间')}</th><th>${t('账号')}</th><th>${t('类型')}</th><th>${t('活动')}</th><th>${t('结果')}</th><th>${t('信息')}</th></tr></thead>
      <tbody>${recs.map(r => `<tr>
        <td style="font-size:12px">${esc(r.created_at)}</td>
        <td>${esc(r.email || ('#' + r.account_id))}</td>
        <td><span class="badge badge-info">${esc(taskLabel(r.task_type))}</span></td>
        <td>${esc(r.plan_name || r.plan_id || '-')}</td>
        <td>${r.success ? `<span class="badge badge-success">${t('成功')}</span>` : `<span class="badge badge-danger">${t('失败')}</span>`}${r.code ? ` <span class="mono" style="font-size:11px">code=${r.code}</span>` : ''}</td>
        <td style="max-width:280px" title="${esc(r.message)}">${esc(r.message || '')}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { reportLoadError(e); }
}

async function loadPlanRuns() {
  try {
    const d = await api('/api/plan-runs?limit=40');
    const el = document.getElementById('planRunsTable');
    const recs = d.records || [];
    if (!recs.length) { el.innerHTML = `<div class="empty"><p>${t('暂无运行记录')}</p></div>`; return; }
    el.innerHTML = `<div class="table-wrap"><table>
      <thead><tr><th>${t('运行时间')}</th><th>${t('计划')}</th><th>${t('任务')}</th><th>${t('状态')}</th><th>${t('成功/失败')}</th><th>${t('耗时')}</th><th>${t('摘要')}</th></tr></thead>
      <tbody>${recs.map(r => `<tr>
        <td style="font-size:12px">${esc(r.run_at)}</td>
        <td>${esc(r.plan_name)}</td>
        <td><span class="badge badge-info">${esc(taskLabel(r.task_type))}</span></td>
        <td>${r.status === 'success' ? `<span class="badge badge-success">${t('成功')}</span>` : `<span class="badge badge-danger">${t('失败')}</span>`}</td>
        <td>${r.success_count} / ${r.fail_count}</td>
        <td>${(r.duration_ms / 1000).toFixed(1)}s</td>
        <td style="max-width:320px" title="${esc(r.message)}">${esc(r.message || '')}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { reportLoadError(e); }
}

// ---- 使用记录 ----

// dailySparkline 按天请求数的纯字符串 SVG 折线（无依赖；不足两个点不画）
function dailySparkline(daily) {
  const vals = (daily || []).map(d => Number(d.requests || 0));
  if (vals.length < 2) return '';
  const max = Math.max(...vals, 1);
  const W = 560, H = 40;
  const step = W / (vals.length - 1);
  const pts = vals.map((v, i) => (i * step).toFixed(1) + ',' + (H - 4 - v / max * (H - 8)).toFixed(1)).join(' ');
  return `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" style="max-width:100%;display:block"><defs><linearGradient id="sparkGrad" x1="0" y1="0" x2="1" y2="0"><stop stop-color="#6366f1"/><stop offset="1" stop-color="#ec4899"/></linearGradient></defs><polyline points="${pts}" fill="none" stroke="url(#sparkGrad)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/></svg>`;
}

async function loadUsageStats() {
  try {
    const u = await api('/api/stats?days=7');
    document.getElementById('uStatReq').textContent = fmtNum(u.requests);
    document.getElementById('uStatIn').textContent = fmtNum(u.prompt_tokens);
    document.getElementById('uStatOut').textContent = fmtNum(u.completion_tokens);
    document.getElementById('uStatDur').textContent = (u.avg_duration_ms || 0) + 'ms';
    document.getElementById('uStatTtft').textContent = (u.avg_ttft_ms || 0) + 'ms';
    document.getElementById('uStatSuccess').textContent = pctText(u.success_rate);
    document.getElementById('uStatCacheHit').textContent = pctText(u.cache_hit_rate);

    // 按下游网关 Key 分布（root 请求的 key_name 为空，后端已归并为 "(root)"）
    const byKey = Object.entries(u.by_gateway_key || {}).sort((a, b) => (b[1].requests || 0) - (a[1].requests || 0));
    document.getElementById('byKeyStats').innerHTML = byKey.length ? `<div class="table-wrap"><table>
      <thead><tr><th>${t('网关 Key')}</th><th>${t('请求数')}</th><th>Tokens</th></tr></thead>
      <tbody>${byKey.map(([id, v]) => `<tr>
        <td>${esc(v.name || ('#' + id))}${id !== '0' ? ` <span class="mono" style="color:var(--c-text-lighter)">#${esc(id)}</span>` : ''}</td>
        <td>${fmtNum(v.requests)}</td>
        <td>${fmtNum(v.tokens)}</td>
      </tr>`).join('')}</tbody></table></div>` : `<div class="empty"><p>${t('暂无数据')}</p></div>`;

    // 按天趋势：sparkline + 明细表
    const daily = u.daily || [];
    const dayEl = document.getElementById('dailyStats');
    if (!dayEl) return;
    if (!daily.length) { dayEl.innerHTML = `<div class="empty"><p>${t('暂无数据')}</p></div>`; return; }
    dayEl.innerHTML = `<div style="margin-bottom:10px">${dailySparkline(daily)}</div>
      <div class="table-wrap"><table>
      <thead><tr><th>${t('日期')}</th><th>${t('请求数')}</th><th>Tokens</th><th>${t('缓存命中')}</th></tr></thead>
      <tbody>${daily.map(d => `<tr>
        <td style="font-size:12px">${esc(d.day)}</td>
        <td>${fmtNum(d.requests)}</td>
        <td>${fmtNum(d.tokens)}</td>
        <td>${d.cache_read_tokens ? fmtNum(d.cache_read_tokens) : '-'}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { reportLoadError(e); }
}

async function loadUsageRecords() {
  try {
    const d = await api('/api/usage-records?limit=100');
    const el = document.getElementById('usageTable');
    const recs = d.records || [];
    if (!recs.length) { el.innerHTML = `<div class="empty"><p>${t('暂无使用记录')}</p></div>`; return; }
    el.innerHTML = `<div class="table-wrap"><table>
      <thead><tr><th>${t('时间')}</th><th>${t('账号')}</th><th>Key</th><th>${t('模型')}</th><th>${t('输入')}</th><th>${t('输出')}</th><th>${t('合计')}</th><th>${t('缓存命中')}</th><th>${t('流式')}</th><th>${t('状态')}</th><th>${t('耗时')}</th><th>TTFT</th></tr></thead>
      <tbody>${recs.map(r => `<tr>
        <td style="font-size:12px">${esc(r.created_at)}</td>
        <td>${esc(r.email || ('#' + r.account_id))}</td>
        <td>${r.key_name ? esc(r.key_name) : '<span style="color:var(--c-text-lighter)">-</span>'}</td>
        <td>${esc(r.model)}</td>
        <td>${fmtNum(r.prompt_tokens)}</td>
        <td>${fmtNum(r.completion_tokens)}</td>
        <td><b>${fmtNum(r.total_tokens)}</b></td>
        <td>${r.cache_read_tokens ? fmtNum(r.cache_read_tokens) : '-'}</td>
        <td>${r.stream ? '✓' : '-'}</td>
        <td>${r.status_code === 200 ? '<span class="badge badge-success">200</span>' : `<span class="badge badge-danger">${r.status_code}</span>`}</td>
        <td>${(r.duration_ms / 1000).toFixed(1)}s</td>
        <td>${r.ttft_ms ? r.ttft_ms + 'ms' : '-'}</td>
      </tr>`).join('')}</tbody></table></div>`;
  } catch (e) { reportLoadError(e); }
}

// ---- 设置 ----

async function loadSettings() {
  try {
    const s = await api('/api/settings');
    document.getElementById('setStrategy').value = s.selection_strategy || 'round_robin';
    document.getElementById('setQuotaInterval').value = s.quota_refresh_interval || '60';
    document.getElementById('setGlobalProxy').value = s.upstream_proxy || '';
    document.getElementById('setCaptchaMode').value = s.captcha_mode || 'auto';
    document.getElementById('setGatewayModels').value = s.gateway_models || '';
    document.getElementById('setSticky').checked = !(s.sticky_sessions === '0' || s.sticky_sessions === 'false');
    document.getElementById('setPromptCacheBreakpoint').checked = s.prompt_cache_breakpoint === '1';
    document.getElementById('setAutoReset').checked = s.auto_reset_enabled === '1';
    document.getElementById('setAutoResetMinWait5h').value = s.auto_reset_min_wait_minutes || '60';
    document.getElementById('setAutoResetMinWaitWeek').value = s.auto_reset_min_wait_week_hours || '24';
    document.getElementById('setAutoResetExpiry').checked = !(s.auto_reset_expiry_enabled === '0' || s.auto_reset_expiry_enabled === 'false');
    document.getElementById('setAutoResetExpirySpend').value = s.auto_reset_expiry_spend_minutes ?? '60';
    document.getElementById('setMaxConcurrent').value = s.max_concurrent_per_account || '3';
    document.getElementById('setPaidFallback').value = s.paid_fallback_mode || 'free_first';
    document.getElementById('setPaidCap').value = s.paid_daily_token_cap || '0';
    window._fpCurrent = s.fingerprint || 'chrome';
    window._ja3Current = s.custom_ja3 || '';
    loadGatewayKey();
    loadModels();
    loadCaptchaStatus();
    loadProxies();
    loadFingerprints();
  } catch (e) { reportLoadError(e); }
}

// ---- TLS 指纹 ----

async function loadFingerprints() {
  try {
    const d = await api('/api/fingerprints');
    const sel = document.getElementById('setFingerprint');
    const groups = {};
    (d.fingerprints || []).forEach(f => { (groups[f.group] = groups[f.group] || []).push(f); });
    sel.innerHTML = Object.entries(groups).map(([g, list]) =>
      `<optgroup label="${esc(g)}">` + list.map(f =>
        `<option value="${esc(f.id)}">${esc(f.label)}</option>`).join('') + '</optgroup>').join('');
    // 选中当前生效值；若不在预置表中（旧库残留）回落到 chrome，避免静默显示第一项
    const cur = window._fpCurrent || 'chrome';
    sel.value = [...sel.options].some(o => o.value === cur) ? cur : 'chrome';
    document.getElementById('setCustomJA3').value = window._ja3Current || '';
    const hint = document.getElementById('fpCurrentHint');
    if (hint) hint.textContent = tf('当前生效: %s（保存后新连接生效）', sel.value);
    fpModeChange();
  } catch (e) { reportLoadError(e); }
}

function fpModeChange() {
  const v = document.getElementById('setFingerprint').value;
  document.getElementById('ja3Wrap').style.display = v === 'custom' ? '' : 'none';
}

async function saveFingerprint() {
  try {
    await api('/api/settings', { method: 'PUT', body: {
      fingerprint: document.getElementById('setFingerprint').value,
      custom_ja3: document.getElementById('setCustomJA3').value.trim(),
    }});
    toast(t('指纹设置已保存（新连接生效）'));
  } catch (e) { toast(e.message, 'error'); }
}

async function saveStrategySettings() {
  try {
    await api('/api/settings', { method: 'PUT', body: {
      selection_strategy: document.getElementById('setStrategy').value,
      quota_refresh_interval: document.getElementById('setQuotaInterval').value,
      sticky_sessions: document.getElementById('setSticky').checked ? '1' : '0',
      prompt_cache_breakpoint: document.getElementById('setPromptCacheBreakpoint').checked ? '1' : '0',
      auto_reset_enabled: document.getElementById('setAutoReset').checked ? '1' : '0',
      auto_reset_min_wait_minutes: document.getElementById('setAutoResetMinWait5h').value || '60',
      auto_reset_min_wait_week_hours: document.getElementById('setAutoResetMinWaitWeek').value || '24',
      auto_reset_expiry_enabled: document.getElementById('setAutoResetExpiry').checked ? '1' : '0',
      auto_reset_expiry_spend_minutes: document.getElementById('setAutoResetExpirySpend').value || '60',
      max_concurrent_per_account: document.getElementById('setMaxConcurrent').value || '3',
      paid_fallback_mode: document.getElementById('setPaidFallback').value || 'free_first',
      paid_daily_token_cap: document.getElementById('setPaidCap').value || '0',
    }});
    toast(t('策略已保存'));
  } catch (e) { toast(e.message, 'error'); }
}

async function saveCaptchaSettings() {
  try {
    await api('/api/settings', { method: 'PUT', body: { captcha_mode: document.getElementById('setCaptchaMode').value } });
    toast(t('验证码设置已保存'));
  } catch (e) { toast(e.message, 'error'); }
}

async function saveModelsSettings() {
  try {
    await api('/api/settings', { method: 'PUT', body: { gateway_models: document.getElementById('setGatewayModels').value.trim() } });
    toast(t('模型清单已保存')); loadModels();
  } catch (e) { toast(e.message, 'error'); }
}

async function loadModels() {
  try {
    const d = await api('/api/models');
    document.getElementById('currentModels').innerHTML =
      t('当前生效: ') + (d.models || []).map(m => `<span class="pill-group" style="margin:2px">${esc(m)}</span>`).join('');
    loadCatalog();
  } catch (e) { reportLoadError(e); }
}

async function loadCatalog() {
  try {
    const d = await api('/api/models/catalog');
    const el = document.getElementById('catalogModels');
    if (!el) return;
    const ms = d.models || [];
    el.innerHTML = ms.length
      ? ms.map(m => `<span class="pill-group" style="margin:2px" title="${esc(`ctx=${m.contextWindow} prio=${m.priority}${m.vision ? ' vision' : ''}`)}">${esc(m.modelId)}</span>`).join('')
      : `<span style="color:var(--c-text-lighter)">${t('尚未同步，点击「同步官方目录」')}</span>`;
  } catch (e) { reportLoadError(e); }
}

async function syncModelCatalog() {
  toast(t('正在同步官方模型目录…'), 'info');
  try {
    const r = await api('/api/models/sync', { method: 'POST' });
    toast(tf('已同步 %s 个模型', r.total));
    loadCatalog();
  } catch (e) { toast(e.message, 'error'); }
}

async function applyCatalogToGateway() {
  try {
    const d = await api('/api/models/catalog');
    const ids = (d.models || []).map(m => m.modelId);
    if (!ids.length) return toast(t('目录为空，请先同步'), 'error');
    await api('/api/settings', { method: 'PUT', body: { gateway_models: ids.join(',') } });
    toast(t('已将目录应用为网关模型清单'));
    loadModels();
  } catch (e) { toast(e.message, 'error'); }
}

// ---- 网关根 Key：GET 只回存在性/脱敏形状，明文需口令步进重认证 ----
// 与导出账号包同一威胁模型：stolen session 不得直接读走根 Key，
// 显示明文必须先过 POST /api/settings/api-key/reveal 的管理员密码验证

let gatewayKeyHas = false; // 最近一次 GET /api/settings/api-key 的存在性（安全页）
let llmKeyHas = false;     // LLM 测试页同一份远端事实，两处各自显示

// applyKeyDisplay 只读输入框统一渲染：未揭示显示占位提示，揭示后填明文
function applyKeyDisplay(inputId, hasKey, revealed) {
  const el = document.getElementById(inputId);
  if (!el) return;
  el.value = revealed || '';
  el.placeholder = hasKey ? t('已设置（点「显示 Key」验证管理员密码后查看）') : t('尚未生成');
}

// ensureRevealKeyBtn 在只读输入框旁注入「显示 Key」按钮（index.html 静态结构不含
// 此按钮，由 JS 补齐；幂等）。data-i18n + applyI18n 让语言切换时文案跟随。
function ensureRevealKeyBtn(inputId, target) {
  const el = document.getElementById(inputId);
  if (!el || document.getElementById('revealKeyBtn-' + target)) return;
  const btn = document.createElement('button');
  btn.id = 'revealKeyBtn-' + target;
  btn.className = 'btn btn-secondary';
  btn.setAttribute('data-i18n', '显示 Key');
  btn.onclick = () => showRevealKeyModal(target);
  el.parentElement.appendChild(btn);
  if (typeof applyI18n === 'function') applyI18n();
}

function showRevealKeyModal(target) {
  openModal(`<h3>${t('显示网关 Key')}</h3>
    <p style="font-size:13px;color:var(--c-text-light);margin-bottom:12px">${t('为防止会话被窃取后直接拿到根 Key，显示前需再次验证管理员密码。')}</p>
    <div class="form-group"><label>${t('管理员密码（确认身份）')}</label><input type="password" id="revealKeyPass" autocomplete="current-password"></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="doRevealKey('${target}')">${t('显示 Key')}</button></div>`);
}

async function doRevealKey(target) {
  const pw = document.getElementById('revealKeyPass').value;
  if (!pw) return toast(t('请输入管理员密码'), 'error');
  try {
    const d = await api('/api/settings/api-key/reveal', { method: 'POST', body: { verify_password: pw } });
    closeModal();
    if (target === 'llm') { llmKeyHas = !!d.has_api_key; applyKeyDisplay('llmKey', llmKeyHas, d.api_key || ''); }
    else { gatewayKeyHas = !!d.has_api_key; applyKeyDisplay('gatewayKeyDisplay', gatewayKeyHas, d.api_key || ''); }
    toast(t('已显示网关 Key'));
  } catch (e) { toast(e.message, 'error'); }
}

async function loadGatewayKey() {
  try {
    const d = await api('/api/settings/api-key');
    gatewayKeyHas = !!d.has_api_key;
    applyKeyDisplay('gatewayKeyDisplay', gatewayKeyHas, '');
    ensureRevealKeyBtn('gatewayKeyDisplay', 'gateway');
  } catch (e) { reportLoadError(e); }
}

async function generateAPIKey() {
  // 生成即轮换并回明文：服务端要求管理员口令步进（纯 session 不得铸新根 Key）
  const pw = prompt(t('重新生成后旧 Key 立即失效；请输入管理员密码确认'));
  if (pw === null) return;
  if (!pw) return toast(t('需要管理员密码'), 'error');
  try {
    const d = await api('/api/settings/api-key/generate', { method: 'POST', body: { verify_password: pw } });
    // 新建即展示一次（与命名网关 Key 的「立即保存」同一模型）
    gatewayKeyHas = true;
    applyKeyDisplay('gatewayKeyDisplay', true, d.api_key || '');
    toast(t('已生成新 API Key'));
  } catch (e) { toast(e.message, 'error'); }
}

function copyGatewayKey() {
  const v = document.getElementById('gatewayKeyDisplay').value;
  if (!v) return toast(t(gatewayKeyHas ? 'Key 已设置，请先「显示 Key」' : '尚未生成'), 'error');
  copyToClipboard(v);
}

// copyToClipboard 剪贴板写入：非安全上下文（如 http://LAN-IP）没有 navigator.clipboard，
// 且 API 存在时也可能被拒（文档未聚焦）——两种失败都要有反馈
function copyToClipboard(text) {
  if (!navigator.clipboard || !navigator.clipboard.writeText) {
    // 降级：选中 弹出提示，让用户手动 Ctrl+C
    toast(t('当前环境不支持自动复制，请手动复制'), 'error');
    return;
  }
  navigator.clipboard.writeText(text).then(() => toast(t('已复制'))).catch(() => toast(t('复制失败，请手动复制'), 'error'));
}

async function changePassword() {
  try {
    await api('/api/auth/password', { method: 'POST', body: {
      old_password: document.getElementById('oldPassword').value,
      new_password: document.getElementById('newPassword').value,
    }});
    toast(t('密码已修改，请重新登录')); setTimeout(doLogout, 1200);
  } catch (e) { toast(e.message, 'error'); }
}

// ---- 验证码状态 ----

async function loadCaptchaStatus() {
  try {
    const c = await api('/api/captcha/status');
    const el = document.getElementById('captchaStatus');
    if (!el) return;
    el.innerHTML = c.has_param
      ? tf('当前参数: %s', c.fresh ? t('✅ 新鲜') : tf('⏳ 已过期 %ss', c.param_age_s)) + (c.config ? ' · scene=' + esc(c.config.scene_id) : '')
      : t('当前无缓存参数（下次请求时自动求解）');
  } catch (e) { reportLoadError(e); }
}

async function solveCaptchaNow() {
  toast(t('正在求解验证码（无头浏览器约 5-15 秒）…'), 'info');
  try {
    const r = await api('/api/captcha/solve', { method: 'POST', body: {} });
    if (r.success) toast(tf('求解成功（参数 %s 字节）', r.param_len));
    else toast(t('求解未返回参数'), 'error');
    loadCaptchaStatus();
  } catch (e) { toast(tf('求解失败: %s', e.message), 'error'); }
}

async function invalidateCaptcha() {
  try { await api('/api/captcha/invalidate', { method: 'POST' }); toast(t('缓存已失效')); loadCaptchaStatus(); }
  catch (e) { toast(e.message, 'error'); }
}

// ---- 出口代理 ----

let proxiesCache = [];

async function loadProxies() {
  try {
    const d = await api('/api/proxies');
    proxiesCache = d.proxies || [];
    renderProxies();
  } catch (e) { reportLoadError(e); }
}

function renderProxies() {
  const el = document.getElementById('proxyTable');
  if (!el) return;
  if (!proxiesCache.length) {
    el.innerHTML = `<div class="empty"><p>${t('暂无代理节点（未配置时全部直连）')}</p></div>`;
    return;
  }
  el.innerHTML = `<div class="table-wrap"><table>
    <thead><tr><th>${t('名称')}</th><th>${t('类型')}</th><th>${t('地址')}</th><th>${t('绑定分组')}</th><th>${t('默认')}</th><th>${t('启用')}</th><th>${t('检测')}</th><th style="width:180px">${t('操作')}</th></tr></thead>
    <tbody>${proxiesCache.map(n => `<tr>
      <td style="font-weight:700">${esc(n.name || '-')}${n.password_broken ? ` <span class="badge badge-danger" title="${t('密码密文无法用当前钥匙解密，节点已被跳过')}">${t('密码损坏')}</span>` : ''}</td>
      <td>${esc(n.type)}</td>
      <td class="mono">${esc(n.host)}:${n.port}${n.username ? ' · ' + esc(n.username) : ''}</td>
      <td>${n.group_name ? n.group_name.split(',').map(g => `<span class="pill-group" style="margin:1px">${esc(g.trim())}</span>`).join('') : '-'}</td>
      <td>${n.is_default ? '⭐' : '-'}</td>
      <td>${n.enabled ? `<span class="badge badge-success">${t('是')}</span>` : `<span class="badge badge-secondary">${t('否')}</span>`}</td>
      <td style="font-size:12px">${n.check_status === 'ok' ? `✅ ${esc(n.check_ip)} ${n.check_latency}ms` : n.check_status === 'fail' ? `❌ ${esc(n.check_msg || '')}` : '-'}
        ${n.check_at ? `<div style="font-size:10.5px;color:var(--c-text-lighter)">${esc(n.check_at)}</div>` : ''}</td>
      <td class="actions-cell">
        <button class="btn btn-sm btn-secondary" onclick="testProxy(${n.id})">${t('测试')}</button>
        <button class="btn btn-sm btn-secondary" onclick="showProxyModal(${n.id})">${t('编辑')}</button>
        <button class="btn btn-sm btn-danger" onclick="deleteProxy(${n.id})">${t('删除')}</button>
      </td>
    </tr>`).join('')}</tbody></table></div>`;
}

function showProxyModal(id) {
  const n = proxiesCache.find(x => x.id === id) || { type: 'socks5', enabled: true };
  openModal(`<h3>${id ? t('编辑代理节点') : t('添加代理节点')}</h3>
    <div class="form-group"><label>${t('名称')}</label><input type="text" id="pxName" value="${esc(n.name || '')}" placeholder="${t('例如：住宅代理-美国')}"></div>
    <div class="form-group"><label>${t('类型')}</label><select id="pxType">
      <option value="socks5" ${n.type === 'socks5' ? 'selected' : ''}>SOCKS5</option>
      <option value="http" ${n.type === 'http' ? 'selected' : ''}>HTTP(S)</option></select></div>
    <div style="display:grid;grid-template-columns:2fr 1fr;gap:10px">
      <div class="form-group"><label>${t('主机')}</label><input type="text" id="pxHost" value="${esc(n.host || '')}" placeholder="127.0.0.1"></div>
      <div class="form-group"><label>${t('端口')}</label><input type="number" id="pxPort" value="${n.port || ''}" placeholder="7897"></div>
    </div>
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
      <div class="form-group"><label>${t('用户名（可选）')}</label><input type="text" id="pxUser" value="${esc(n.username || '')}"></div>
      <div class="form-group"><label>${t('密码（可选）')}</label><input type="password" id="pxPass" placeholder="${id ? t('留空保持不变') : ''}"></div>
    </div>
    <div class="form-group"><label>${t('绑定分组（逗号分隔，留空=不绑定）')}</label><input type="text" id="pxGroup" value="${esc(n.group_name || '')}" placeholder="default,work"></div>
    <div class="form-group"><label>
      <input type="checkbox" id="pxDefault" ${n.is_default ? 'checked' : ''} style="width:auto;margin-right:6px">${t('设为默认节点（未绑定组的账号走它）')}
      <input type="checkbox" id="pxEnabled" ${n.enabled !== false ? 'checked' : ''} style="width:auto;margin:0 6px 0 16px">${t('启用')}</label></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="saveProxy(${id || 0})">${t('保存')}</button></div>`);
}

async function saveProxy(id) {
  const body = {
    name: document.getElementById('pxName').value.trim(),
    type: document.getElementById('pxType').value,
    host: document.getElementById('pxHost').value.trim(),
    port: Number(document.getElementById('pxPort').value || 0),
    username: document.getElementById('pxUser').value.trim(),
    password: document.getElementById('pxPass').value,
    group_name: document.getElementById('pxGroup').value.trim(),
    is_default: document.getElementById('pxDefault').checked,
    enabled: document.getElementById('pxEnabled').checked,
  };
  try {
    if (id) await api('/api/proxies/' + id, { method: 'PUT', body });
    else await api('/api/proxies', { method: 'POST', body });
    closeModal(); toast(t('已保存')); loadProxies(); loadGroups();
  } catch (e) { toast(e.message, 'error'); }
}

async function deleteProxy(id) {
  if (!confirm(t('确认删除该代理节点？'))) return;
  try { await api('/api/proxies/' + id, { method: 'DELETE' }); toast(t('已删除')); loadProxies(); }
  catch (e) { toast(e.message, 'error'); }
}

async function testProxy(id) {
  toast(t('测试中…'), 'info');
  try {
    const r = await api(`/api/proxies/${id}/test`, { method: 'POST' });
    if (r.ok) toast(tf('出口 IP: %s（%sms）', r.exit_ip, r.elapsed_ms));
    else toast(tf('测试失败: %s', r.message || ''), 'error');
    loadProxies();
  } catch (e) { toast(tf('测试失败: %s', e.message), 'error'); }
}

async function probeProxyPorts() {
  try {
    const d = await api('/api/proxies/probe-ports');
    const ports = d.ports || [];
    if (!ports.length) return toast(t('未发现开放的本机代理端口'), 'error');
    openModal(`<h3>${t('本机代理端口探测')}</h3>
      ${ports.map(p => `<div style="display:flex;justify-content:space-between;align-items:center;padding:9px 4px;border-bottom:1px solid var(--c-border-light)">
        <div><b class="mono">${esc(p.url)}</b><div style="font-size:11.5px;color:var(--c-text-light)">${esc(p.label)}</div></div>
        <button class="btn btn-sm btn-primary" data-url="${esc(p.url)}" onclick="useProbedPort(this.dataset.url)">${t('使用')}</button></div>`).join('')}
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('关闭')}</button></div>`);
  } catch (e) { toast(e.message, 'error'); }
}

function useProbedPort(url) {
  closeModal();
  document.getElementById('setGlobalProxy').value = url;
  saveGlobalProxy();
}

async function detectSystemProxy() {
  try {
    const d = await api('/api/proxies/system');
    if (d.enabled && d.url) {
      document.getElementById('setGlobalProxy').value = d.url;
      toast(tf('已填入系统代理: %s', d.url), 'info');
    } else toast(t('系统未启用代理'), 'error');
  } catch (e) { toast(e.message, 'error'); }
}

async function saveGlobalProxy() {
  try {
    await api('/api/settings', { method: 'PUT', body: { upstream_proxy: document.getElementById('setGlobalProxy').value.trim() } });
    toast(t('全局代理已保存'));
  } catch (e) { toast(e.message, 'error'); }
}

async function testGlobalProxy() {
  const url = document.getElementById('setGlobalProxy').value.trim();
  const el = document.getElementById('globalProxyTest');
  el.textContent = t('测试中…');
  try {
    const r = await api('/api/proxies/test-url', { method: 'POST', body: { url } });
    el.innerHTML = r.ok
      ? tf('✅ 出口 IP: <b>%s</b>（%sms）', esc(r.exit_ip), r.elapsed_ms)
      : tf('❌ %s', esc(r.message || t('不可用')));
  } catch (e) { el.textContent = '❌ ' + e.message; }
}

// ---- 加密账号包迁移 ----

function showExportBundleModal() {
  openModal(`<h3>${t('导出加密账号包')}</h3>
    <p style="font-size:13px;color:var(--c-text-light);margin-bottom:12px">${t('导出全部账号的凭证（JWT / API Key / 设备指纹 / 本地快照），PBKDF2+AES-256-GCM 加密，可跨机器导入。')}</p>
    <div class="form-group"><label>${t('加密密码')}</label><input type="password" id="expPass" placeholder="${t('导入时需要相同密码')}"></div>
    <div class="form-group"><label>${t('管理员密码（确认身份）')}</label><input type="password" id="expAdminPass" autocomplete="current-password"></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="doExportBundle()">${t('生成账号包')}</button></div>`);
}

async function doExportBundle() {
  const pass = document.getElementById('expPass').value;
  const adminPass = document.getElementById('expAdminPass').value;
  if (!pass) return toast(t('请设置密码'), 'error');
  if (!adminPass) return toast(t('请输入管理员密码'), 'error');
  try {
    const r = await api('/api/accounts/export', { method: 'POST', body: { password: pass, admin_password: adminPass } });
    openModal(`<h3>${t('账号包已生成')}</h3>
      <div class="form-group"><textarea id="bundleText" style="min-height:160px">${esc(r.bundle)}</textarea></div>
      <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('关闭')}</button>
      <button class="btn btn-primary" onclick="copyToClipboard(document.getElementById('bundleText').value)">${t('复制')}</button></div>`);
  } catch (e) { toast(e.message, 'error'); }
}

function showImportBundleModal() {
  openModal(`<h3>${t('导入加密账号包')}</h3>
    <div class="form-group"><label>${t('解密密码')}</label><input type="password" id="impPass"></div>
    <div class="form-group"><label>${t('账号包内容（zcb1: 开头）')}</label><textarea id="impBundle" placeholder="zcb1:..."></textarea></div>
    <div class="actions"><button class="btn btn-secondary" onclick="closeModal()">${t('取消')}</button>
    <button class="btn btn-primary" onclick="doImportBundle()">${t('导入')}</button></div>`);
}

async function doImportBundle() {
  try {
    const r = await api('/api/accounts/import/bundle', { method: 'POST', body: {
      password: document.getElementById('impPass').value,
      bundle: document.getElementById('impBundle').value.trim(),
    }});
    closeModal(); toast(tf('导入成功：%s 个账号', r.imported)); switchSection('accounts');
  } catch (e) { toast(e.message, 'error'); }
}

// ---- LLM 测试 ----

let llmHistory = [];

async function loadLlmKey() {
  try {
    const d = await api('/api/settings/api-key');
    llmKeyHas = !!d.has_api_key;
    applyKeyDisplay('llmKey', llmKeyHas, '');
    ensureRevealKeyBtn('llmKey', 'llm');
  } catch (e) { reportLoadError(e); }
}

// 从 SSE 行中提取增量文本（按协议）
function llmDeltaText(proto, data) {
  try {
    const j = JSON.parse(data);
    if (proto === 'messages') {
      if (j.type === 'content_block_delta' && j.delta && j.delta.type === 'text_delta') return j.delta.text || '';
      return '';
    }
    if (proto === 'chat') {
      const c = (j.choices || [])[0];
      return (c && c.delta && c.delta.content) || '';
    }
    if (proto === 'responses') {
      if (j.type === 'response.output_text.delta') return j.delta || '';
      return '';
    }
  } catch (e) { /* 非 JSON 增量帧按空处理 */ }
  return '';
}

// 从 SSE 行中提取增量思考文本（思考型模型可能只输出 thinking）
function llmDeltaThink(proto, data) {
  try {
    const j = JSON.parse(data);
    if (proto === 'messages') {
      if (j.type === 'content_block_delta' && j.delta && j.delta.type === 'thinking_delta') return j.delta.thinking || '';
      return '';
    }
    if (proto === 'chat') {
      const c = (j.choices || [])[0];
      return (c && c.delta && c.delta.reasoning_content) || '';
    }
    if (proto === 'responses') {
      if (j.type === 'response.reasoning_summary_text.delta') return j.delta || '';
      return '';
    }
  } catch (e) { /* 非 JSON 增量帧按空处理 */ }
  return '';
}

function llmUsageFrom(json, proto) {
  const u = json && json.usage;
  if (!u) return null;
  if (proto === 'chat') return { in: u.prompt_tokens, out: u.completion_tokens };
  if (proto === 'responses') return { in: u.input_tokens, out: u.output_tokens };
  return { in: u.input_tokens, out: u.output_tokens };
}

async function runLlmTest() {
  const proto = document.getElementById('llmProto').value;
  const model = document.getElementById('llmModel').value.trim() || 'GLM-4.5-Flash';
  const maxTokens = Number(document.getElementById('llmMaxTokens').value || 256);
  const stream = document.getElementById('llmStream').checked;
  const prompt = document.getElementById('llmPrompt').value || t('你好');
  const key = document.getElementById('llmKey').value;
  const btn = document.getElementById('llmRunBtn');
  const resultEl = document.getElementById('llmResult');
  const contentEl = document.getElementById('llmContent');
  if (!key) {
    toast(t(llmKeyHas ? 'Key 已设置，请先「显示 Key」' : '缺少网关 Key，点击「刷新 Key」'), 'error');
    return;
  }

  const path = proto === 'messages' ? '/v1/messages' : proto === 'chat' ? '/v1/chat/completions' : '/v1/responses';
  let body;
  if (proto === 'messages') body = { model, max_tokens: maxTokens, stream, messages: [{ role: 'user', content: prompt }] };
  else if (proto === 'chat') body = { model, max_tokens: maxTokens, stream, messages: [{ role: 'user', content: prompt }] };
  else body = { model, max_output_tokens: maxTokens, stream, input: prompt };
  const headers = { 'Content-Type': 'application/json' };
  if (proto === 'messages') headers['x-api-key'] = key; else headers['Authorization'] = 'Bearer ' + key;

  btn.disabled = true;
  resultEl.innerHTML = `<div class="kv-item"><div class="k">${t('状态')}</div><div class="v">${t('运行中…')}</div></div>`;
  contentEl.textContent = '';
  const t0 = performance.now();
  let ttft = 0, text = '', think = '', events = 0, usage = null, status = 0, streamError = '';
  try {
    const resp = await fetch(path, { method: 'POST', headers, body: JSON.stringify(body) });
    status = resp.status;
    // 非 2xx 的 JSON 错误体（鉴权/RPM/配额拒绝）必须走容错解析分支：
    // SSE 解析器消费纯 JSON 体只会得到零帧，把真实拒绝原因吞成"（空响应）"
    if (stream && resp.ok && resp.body) {
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        if (!ttft) ttft = Math.round(performance.now() - t0);
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, idx); buf = buf.slice(idx + 2);
          const dl = frame.split('\n').find(l => l.startsWith('data:'));
          if (!dl) continue;
          const data = dl.slice(5).trim();
          if (!data || data === '[DONE]') continue;
          events++;
          text += llmDeltaText(proto, data);
          think += llmDeltaThink(proto, data);
          try {
            const j = JSON.parse(data);
            const u = llmUsageFrom(j, proto);
            if (u && (u.in || u.out)) usage = u;
            // 网关把流内错误以 SSE 帧透传（event:error / error 键）：
            // 不识别会把失败流记成"测试完成"
            if (j.type === 'error' || j.error) {
              const em = (j.error && (j.error.message || j.error.type)) || 'stream error';
              streamError = em;
            }
          } catch (e) { }
        }
      }
    } else {
      // 容错解析：非 JSON 错误体（反代 502 HTML、空 body）不得把真实 HTTP 状态
      // 误报成"网络错误"
      let j = null;
      try { j = await resp.json(); } catch (e) { /* keep null */ }
      if (!ttft) ttft = Math.round(performance.now() - t0);
      if (resp.ok && j) {
        if (proto === 'messages') {
          text = (j.content || []).filter(c => c.type === 'text').map(c => c.text).join('');
          think = (j.content || []).filter(c => c.type === 'thinking').map(c => c.thinking).join('');
        } else if (proto === 'chat') {
          const m = ((j.choices || [])[0] || {}).message || {};
          text = m.content || '';
          think = m.reasoning_content || '';
        } else {
          text = j.output_text || '';
          think = (j.output || []).filter(o => o.type === 'reasoning').map(o => (o.summary || []).map(s => s.text).join('')).join('');
        }
        usage = llmUsageFrom(j, proto);
      } else if (j) {
        text = JSON.stringify(j).slice(0, 500);
      } else {
        text = `HTTP ${resp.status}（非 JSON 响应体）`;
      }
    }
  } catch (e) {
    // status 在 fetch 成功后已保有真实 HTTP 码；走到这里说明请求本身失败
    // （流中途断连 reader.read() reject 也落这里）——必须进失败判定，
    // 否则 200 起始的流挂掉会绿灯"测试完成"
    text = tf('请求失败: %s', e.message);
    streamError = e.message;
  }
  const latency = Math.round(performance.now() - t0);
  btn.disabled = false;
  const ok = status >= 200 && status < 300 && !streamError;
  resultEl.innerHTML = `
    <div class="kv-item"><div class="k">${t('HTTP 状态')}</div><div class="v" style="color:${ok ? 'var(--c-success-dark)' : 'var(--c-danger)'}">${status || t('网络错误')}</div></div>
    <div class="kv-item"><div class="k">${t('总延迟')}</div><div class="v">${latency}ms</div></div>
    <div class="kv-item"><div class="k">${t('首字 TTFT')}</div><div class="v">${stream ? ttft + 'ms' : '-'}</div></div>
    <div class="kv-item"><div class="k">Tokens in/out</div><div class="v">${usage ? `${usage.in ?? '-'} / ${usage.out ?? '-'}` : '-'}</div></div>
    <div class="kv-item"><div class="k">${t('SSE 事件数')}</div><div class="v">${stream ? events : '-'}</div></div>
    ${streamError ? `<div class="kv-item"><div class="k">${t('流内错误')}</div><div class="v" style="color:var(--c-danger)">${esc(streamError)}</div></div>` : ''}
    <div class="kv-item"><div class="k">${t('协议')}</div><div class="v">${proto}${stream ? ' (stream)' : ''}</div></div>`;
  contentEl.textContent = text
    ? text
    : (think ? t('【模型仅输出思考过程（max_tokens 不足或未产出正文）】') + '\n' + think : t('（空响应）'));
  contentEl.dataset.placeholderLang = ''; // 真实输出：不再是占位文案
  llmHistory.unshift({ t: new Date().toLocaleTimeString(), proto, model, status, latency, ttft, ok });
  document.getElementById('llmHistory').innerHTML = llmHistory.slice(0, 10).map(h =>
    `<div>${esc(h.t)} · ${esc(h.proto)} · ${esc(h.model)} · <span style="color:${h.ok ? 'var(--c-success-dark)' : 'var(--c-danger)'}">${h.status}</span> · ${h.latency}ms${h.ttft ? ' / ttft ' + h.ttft + 'ms' : ''}</div>`).join('');
  if (!ok) toast(streamError ? tf('流内错误: %s', streamError) : tf('测试返回 %s', status), 'error');
  else toast(t('测试完成'));
}

function clearLlmHistory() {
  llmHistory = [];
  const h = document.getElementById('llmHistory');
  h.innerHTML = t('（无）');
  h.dataset.placeholderLang = CURRENT_LANG;
  document.getElementById('llmResult').innerHTML = '';
  const c = document.getElementById('llmContent');
  c.textContent = t('（尚未运行）');
  c.dataset.placeholderLang = CURRENT_LANG;
}

// llmContent/llmHistory 不带 data-i18n（语言切换会覆盖真实测试输出），
// 占位文案按当前语言写入；语言切换后占位跟随更新（真实输出不受影响）
function initLlmPlaceholders() {
  const apply = (el, text) => {
    if (!el) return;
    if (el.dataset.placeholderLang === '') return;         // 已是真实输出，不覆盖
    if (el.dataset.placeholderLang === CURRENT_LANG) return; // 占位已是当前语言
    // undefined = 首次加载的空白元素：写入当前语言的占位
    el.textContent = text;
    el.dataset.placeholderLang = CURRENT_LANG;
  };
  apply(document.getElementById('llmContent'), t('（尚未运行）'));
  const h = document.getElementById('llmHistory');
  // undefined = 首次加载的空白元素：与 apply() 同路径写占位（原条件恒 false，
  // 历史面板首次打开一直空白）
  if (h && !llmHistory.length && h.dataset.placeholderLang === undefined) {
    h.textContent = t('（无）');
    h.dataset.placeholderLang = CURRENT_LANG;
  }
}

// setLlmPrompt 快捷提示词填入
function setLlmPrompt(text) {
  const el = document.getElementById('llmPrompt');
  el.value = text;
  el.focus();
}

// ---- 启动 ----

function loadAll() {
  loadDashboard();
  loadGroups().then(loadAccounts);
  initLlmPlaceholders();
  syncLlmPromptDefault();
}

setInterval(() => {
  if (document.getElementById('mainApp').style.display !== 'none') {
    if (document.getElementById('section-dashboard').classList.contains('active')) loadDashboard();
    if (document.getElementById('section-activity').classList.contains('active')) { pollRunning(); }
  }
}, 5000);

checkAuth();
