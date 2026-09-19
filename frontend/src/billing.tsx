// Billing views: the usage line under a reply, the running session total, the
// usage reports (a user's own, and the administrator's for everyone), and the
// model price table.
//
// Every figure here comes from the server's ledger, which prices each model
// call when it finishes. Token counts never overlap — "输入" is the uncached
// part of the prompt, "缓存" the part served from the provider's cache — so the
// four columns add up to what the model processed.
//
// Like admin.tsx, the panels take `api` as a prop instead of reaching for the
// context in App.tsx, so this module has no import cycle with it.
import { useCallback, useEffect, useMemo, useState } from "react";
import type {
  CallUsage,
  ModelInfo,
  ModelPrice,
  PigoAPI,
  PriceList,
  ProviderInfo,
  TurnUsage,
  UsageGroup,
  UsageQuery,
  UsageReport,
  UsageTotals,
} from "./api";
import { navigate } from "./route";
import { PanelHeading, errorText, usePanelMessage } from "./panel";

// --- formatting ------------------------------------------------------------------

// formatYuan shows four decimals, as agreed: small calls are fractions of a fen,
// and rounding them to zero would make them look free.
export function formatYuan(yuan: number): string {
  if (yuan > 0 && yuan < 0.0001) return "<¥0.0001";
  return `¥${yuan.toFixed(4)}`;
}

export function formatTokens(count: number): string {
  if (count >= 1_000_000) return `${(count / 1_000_000).toFixed(count >= 10_000_000 ? 0 : 1)}M`;
  if (count >= 1000) return `${(count / 1000).toFixed(count >= 10_000 ? 0 : 1)}k`;
  return String(count);
}

const statusLabel: Record<string, string> = {
  ok: "完成",
  error: "出错（上游已计费）",
  aborted: "中断，用量未知",
  unreported: "上游未报告用量",
};

const kindLabel: Record<string, string> = { chat: "对话", compaction: "上下文压缩" };

// totalsOf adds calls up the way the server does, for a turn still streaming.
export function totalsOf(calls: CallUsage[]): TurnUsage {
  const totals: TurnUsage = {
    calls: 0, input: 0, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 0,
    cost: 0, platformCost: 0, selfCost: 0, unpriced: 0, incomplete: 0, details: calls,
  };
  for (const call of calls) {
    totals.calls++;
    totals.input += call.input;
    totals.cacheRead += call.cacheRead;
    totals.cacheWrite += call.cacheWrite;
    totals.output += call.output;
    totals.reasoning += call.reasoning ?? 0;
    totals.cost += call.cost;
    if (call.billedTo === "self") totals.selfCost += call.cost;
    else totals.platformCost += call.cost;
    if (!call.priced) totals.unpriced++;
    if (call.status === "aborted" || call.status === "unreported") totals.incomplete++;
  }
  return totals;
}

function callDetail(call: CallUsage): string {
  const parts = [
    `${kindLabel[call.kind] ?? call.kind} · ${call.provider}/${call.model}`,
    `  输入 ${call.input} · 缓存命中 ${call.cacheRead}${call.cacheWrite ? ` · 缓存写入 ${call.cacheWrite}` : ""} · 输出 ${call.output}${call.reasoning ? `（含推理 ${call.reasoning}）` : ""}`,
    `  ${call.priced ? formatYuan(call.cost) : "未定价"}${call.billedTo === "self" ? " · 自付" : ""}${call.status !== "ok" ? ` · ${statusLabel[call.status] ?? call.status}` : ""}`,
  ];
  return parts.join("\n");
}

// --- the line under a reply ----------------------------------------------------------

// UsageLine sums every model call of the turn — tool rounds and context
// compaction included — into one line; hovering lists the calls.
export function UsageLine({ usage }: { usage: TurnUsage }) {
  const self = usage.selfCost > 0 && usage.platformCost === 0;
  const title = usage.details.map(callDetail).join("\n\n");
  return (
    <div className="usage-line" title={title}>
      <span>输入 {formatTokens(usage.input)}</span>
      {usage.cacheRead > 0 && <span>缓存 {formatTokens(usage.cacheRead)}</span>}
      {usage.cacheWrite > 0 && <span>写缓存 {formatTokens(usage.cacheWrite)}</span>}
      <span>输出 {formatTokens(usage.output)}</span>
      {usage.calls > 1 && <span>{usage.calls} 次调用</span>}
      <strong>{usage.unpriced === usage.calls ? "未定价" : formatYuan(usage.cost)}</strong>
      {self && <em>自付</em>}
      {usage.unpriced > 0 && usage.unpriced < usage.calls && <em>部分未定价</em>}
      {usage.incomplete > 0 && <em className="usage-warn">用量不完整</em>}
    </div>
  );
}

// SessionCost is the running total beside the model picker.
export function SessionCost({ totals }: { totals: UsageTotals | null }) {
  if (!totals || totals.calls === 0) return null;
  const title = [
    `本会话 ${totals.calls} 次模型调用`,
    `输入 ${totals.input} · 缓存命中 ${totals.cacheRead} · 缓存写入 ${totals.cacheWrite} · 输出 ${totals.output}`,
    totals.selfCost > 0 ? `其中自付 ${formatYuan(totals.selfCost)}` : "",
    totals.unpriced > 0 ? `${totals.unpriced} 次调用的模型未定价，按 0 计` : "",
  ].filter(Boolean).join("\n");
  return (
    <button type="button" className="session-cost" title={title} onClick={() => navigate("/settings/usage")}>
      {formatYuan(totals.cost)}
    </button>
  );
}

// --- usage reports -----------------------------------------------------------------

function localDate(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function monthStart(): string {
  const now = new Date();
  return localDate(new Date(now.getFullYear(), now.getMonth(), 1));
}

const presets: { label: string; range(): [string, string] }[] = [
  { label: "今天", range: () => [localDate(new Date()), localDate(new Date())] },
  { label: "近 7 天", range: () => [localDate(new Date(Date.now() - 6 * 86400_000)), localDate(new Date())] },
  { label: "本月", range: () => [monthStart(), localDate(new Date())] },
  {
    label: "上月",
    range: () => {
      const now = new Date();
      return [localDate(new Date(now.getFullYear(), now.getMonth() - 1, 1)), localDate(new Date(now.getFullYear(), now.getMonth(), 0))];
    },
  },
];

function TotalsGrid({ totals }: { totals: UsageTotals }) {
  return (
    <div className="metric-grid usage-metrics">
      <div className="metric"><strong>{formatYuan(totals.cost)}</strong><span>费用（{totals.calls} 次调用）</span></div>
      <div className="metric"><strong>{formatTokens(totals.input + totals.cacheRead + totals.cacheWrite)}</strong><span>总输入（含缓存命中 {formatTokens(totals.cacheRead)}）</span></div>
      <div className="metric"><strong>{formatTokens(totals.output)}</strong><span>输出{totals.reasoning ? `（推理 ${formatTokens(totals.reasoning)}）` : ""}</span></div>
      <div className="metric"><strong>{formatYuan(totals.platformCost)}</strong><span>平台 Key 支出</span></div>
      <div className="metric"><strong>{formatYuan(totals.selfCost)}</strong><span>自带 Key 支出</span></div>
      <div className="metric">
        <strong>{totals.incomplete}</strong>
        <span>用量未知的调用</span>
      </div>
    </div>
  );
}

function GroupTable({ title, rows, keyLabel, onPick }: {
  title: string;
  rows: UsageGroup[];
  keyLabel: string;
  onPick?(row: UsageGroup): void;
}) {
  if (rows.length === 0) return null;
  return (
    <div className="usage-block">
      <h4>{title}</h4>
      <div className="usage-table-wrap">
        <table className="usage-table">
          <thead>
            <tr>
              <th>{keyLabel}</th><th>调用</th><th>输入</th><th>缓存命中</th><th>输出</th><th>费用</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.key}>
                <td>
                  {onPick ? (
                    <button type="button" className="usage-link" onClick={() => onPick(row)}>{row.label || row.key}</button>
                  ) : (
                    row.label || row.key
                  )}
                  {row.deleted && <em className="usage-tag">已删除用户</em>}
                  {row.unpriced > 0 && <em className="usage-tag">未定价 {row.unpriced}</em>}
                </td>
                <td>{row.calls}</td>
                <td>{formatTokens(row.input + row.cacheWrite)}</td>
                <td>{formatTokens(row.cacheRead)}</td>
                <td>{formatTokens(row.output)}</td>
                <td>{formatYuan(row.cost)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function RecentTable({ report, admin }: { report: UsageReport; admin: boolean }) {
  if (report.recent.length === 0) return null;
  return (
    <div className="usage-block">
      <h4>最近调用{report.recent.length >= 200 ? "（最新 200 条，完整明细请导出 CSV）" : ""}</h4>
      <div className="usage-table-wrap">
        <table className="usage-table">
          <thead>
            <tr>
              <th>时间</th>{admin && <th>用户</th>}<th>模型</th><th>类型</th><th>输入</th><th>缓存</th><th>输出</th><th>费用</th>
            </tr>
          </thead>
          <tbody>
            {report.recent.map((entry) => (
              <tr key={entry.id} className={entry.status !== "ok" ? "usage-row-muted" : undefined}>
                <td>{new Date(entry.at).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })}</td>
                {admin && <td>{entry.username || entry.userId || "—"}</td>}
                <td title={`${entry.provider}/${entry.model}${entry.responseModel && entry.responseModel !== entry.model ? ` → ${entry.responseModel}` : ""}`}>
                  {entry.responseModel || entry.model}
                </td>
                <td>
                  {kindLabel[entry.kind] ?? entry.kind}
                  {entry.status !== "ok" && <em className="usage-tag">{statusLabel[entry.status] ?? entry.status}</em>}
                  {entry.billedTo === "self" && <em className="usage-tag">自付</em>}
                </td>
                <td>{entry.input}</td>
                <td>{entry.cacheRead + entry.cacheWrite}</td>
                <td>{entry.output}</td>
                <td>{entry.priced ? formatYuan(entry.costYuan) : "未定价"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// UsagePanel is the personal report, or with `admin` the deployment-wide one:
// the same figures, plus a per-user breakdown, a user filter, and the list of
// models that were used without a price.
export function UsagePanel({ api, admin = false }: { api: PigoAPI; admin?: boolean }) {
  const [from, setFrom] = useState(monthStart);
  const [to, setTo] = useState(() => localDate(new Date()));
  const [user, setUser] = useState<{ id: string; name: string } | null>(null);
  const [report, setReport] = useState<UsageReport | null>(null);
  const [loading, setLoading] = useState(false);
  const { report: say, view } = usePanelMessage();

  const query = useMemo<UsageQuery>(() => ({ from, to, user: user?.id }), [from, to, user]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setReport(await api.usage(query, admin));
      say("", false);
    } catch (cause) {
      say(errorText(cause), true);
    } finally {
      setLoading(false);
    }
  }, [api, query, admin, say]);

  useEffect(() => {
    void load();
  }, [load]);

  async function exportCSV() {
    try {
      await api.downloadUsage(query, admin);
    } catch (cause) {
      say(errorText(cause), true);
    }
  }

  return (
    <section className="settings-section">
      <PanelHeading
        title={admin ? "全员用量" : "我的用量"}
        hint={
          admin
            ? "按账本统计所有用户的模型调用。已删除用户的记录保留。"
            : "你的每次模型调用都会记录 token 和费用，含工具调用轮次和上下文压缩。"
        }
        icon="¥"
      />
      <div className="usage-filters">
        <label>从 <input type="date" className="settings-input" value={from} max={to} onChange={(event) => setFrom(event.target.value)} /></label>
        <label>到 <input type="date" className="settings-input" value={to} min={from} onChange={(event) => setTo(event.target.value)} /></label>
        <div className="usage-presets">
          {presets.map((preset) => (
            <button
              key={preset.label}
              type="button"
              className="ghost-button"
              onClick={() => {
                const [start, end] = preset.range();
                setFrom(start);
                setTo(end);
              }}
            >
              {preset.label}
            </button>
          ))}
        </div>
        {user && (
          <button type="button" className="ghost-button active" onClick={() => setUser(null)} title="清除用户筛选">
            用户：{user.name} ×
          </button>
        )}
        <button type="button" className="ghost-button" disabled={loading} onClick={() => void exportCSV()}>导出 CSV</button>
      </div>

      {report && (
        <>
          <TotalsGrid totals={report.totals} />
          {report.totals.calls === 0 && <p className="credential-note">这段时间没有模型调用。</p>}
          {admin && report.unpriced.length > 0 && (
            <div className="usage-unpriced">
              <strong>以下模型被使用过但没有价格，按 0 元计：</strong>
              {report.unpriced.map((item) => (
                <span key={`${item.provider}/${item.model}`}>
                  {item.provider}/{item.model}（{item.calls} 次）
                </span>
              ))}
              <button type="button" className="ghost-button" onClick={() => navigate("/settings/prices")}>去定价</button>
            </div>
          )}
          {admin && !user && (
            <GroupTable
              title="按用户"
              keyLabel="用户"
              rows={report.byUser ?? []}
              onPick={(row) => setUser({ id: row.key, name: row.label || row.key })}
            />
          )}
          <GroupTable title="按模型" keyLabel="provider/模型" rows={report.byModel} />
          <GroupTable title="按日" keyLabel="日期" rows={report.byDay} />
          <RecentTable report={report} admin={admin} />
        </>
      )}
      {view}
    </section>
  );
}

// --- the price table -----------------------------------------------------------

type PriceDraft = { provider: string; model: string; input: string; cacheRead: string; cacheWrite: string; output: string };

const emptyDraft: PriceDraft = { provider: "", model: "", input: "", cacheRead: "", cacheWrite: "", output: "" };

function priceText(value: number | undefined, fallback?: number): string {
  if (value === undefined) return fallback === undefined ? "—" : `${fallback}（同输入）`;
  return String(value);
}

// Prices is readable by everyone — knowing what a model costs helps pick one —
// and editable by administrators.
export function Prices({ api, isAdmin, providers, models }: {
  api: PigoAPI;
  isAdmin: boolean;
  providers: ProviderInfo[];
  models: ModelInfo[];
}) {
  const [list, setList] = useState<PriceList | null>(null);
  const [draft, setDraft] = useState<PriceDraft>(emptyDraft);
  const { report, view } = usePanelMessage();

  useEffect(() => {
    void api.prices().then(setList).catch((cause) => report(errorText(cause), true));
  }, [api, report]);

  function edit(price: ModelPrice) {
    setDraft({
      provider: price.provider,
      model: price.model,
      input: String(price.input),
      cacheRead: price.cacheRead === undefined ? "" : String(price.cacheRead),
      cacheWrite: price.cacheWrite === undefined ? "" : String(price.cacheWrite),
      output: String(price.output),
    });
  }

  async function save() {
    const number = (text: string) => (text.trim() === "" ? undefined : Number(text));
    const input = number(draft.input);
    const output = number(draft.output);
    if (!draft.provider.trim() || !draft.model.trim() || input === undefined || output === undefined) {
      report("provider、模型、输入价和输出价都要填", true);
      return;
    }
    const values = [input, output, number(draft.cacheRead), number(draft.cacheWrite)];
    if (values.some((value) => value !== undefined && (!Number.isFinite(value) || value < 0))) {
      report("单价须为非负数字", true);
      return;
    }
    try {
      setList(
        await api.putPrice({
          provider: draft.provider.trim(),
          model: draft.model.trim(),
          input,
          output,
          cacheRead: number(draft.cacheRead),
          cacheWrite: number(draft.cacheWrite),
        }),
      );
      report(`已保存 ${draft.provider}/${draft.model} 的价格，之后的调用按新价格计`, false);
      setDraft(emptyDraft);
    } catch (cause) {
      report(errorText(cause), true);
    }
  }

  async function remove(price: ModelPrice) {
    if (!window.confirm(`删除 ${price.provider}/${price.model} 的价格？之后该模型的调用按 0 元计（已记录的不变）。`)) return;
    try {
      setList(await api.deletePrice(price.provider, price.model));
      report("已删除", false);
    } catch (cause) {
      report(errorText(cause), true);
    }
  }

  const modelChoices = models.filter((item) => !draft.provider || item.provider === draft.provider);

  return (
    <section className="settings-section">
      <PanelHeading
        title="模型价格"
        hint="单位：元 / 百万 token。改价只影响之后的调用；每条记录保存计费时的单价。"
        icon="价"
      />
      {list && list.prices.length === 0 && <p className="credential-note">还没有配置任何价格，所有调用都按 0 元记录。</p>}
      {list && list.prices.length > 0 && (
        <div className="usage-table-wrap">
          <table className="usage-table">
            <thead>
              <tr>
                <th>provider/模型</th><th>输入</th><th>缓存命中</th><th>缓存写入</th><th>输出</th>{isAdmin && <th />}
              </tr>
            </thead>
            <tbody>
              {list.prices.map((price) => (
                <tr key={`${price.provider}/${price.model}`}>
                  <td title={price.updatedBy ? `${price.updatedBy} 修改于 ${new Date(price.updatedAt ?? "").toLocaleString("zh-CN")}` : undefined}>
                    {price.provider}/{price.model}
                  </td>
                  <td>{price.input}</td>
                  <td>{priceText(price.cacheRead, price.input)}</td>
                  <td>{priceText(price.cacheWrite, price.input)}</td>
                  <td>{price.output}</td>
                  {isAdmin && (
                    <td className="usage-actions">
                      <button type="button" className="ghost-button" onClick={() => edit(price)}>修改</button>
                      <button type="button" className="ghost-button" onClick={() => void remove(price)}>删除</button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {isAdmin && (
        <div className="price-form">
          <h4>添加 / 修改价格</h4>
          <div className="price-form-grid">
            <label>
              provider
              <select className="settings-input settings-select" value={draft.provider} onChange={(event) => setDraft({ ...draft, provider: event.target.value })}>
                <option value="">选择 provider</option>
                {providers.map((item) => (
                  <option key={item.name} value={item.name}>{item.name}{item.custom ? "（自定义）" : ""}</option>
                ))}
              </select>
            </label>
            <label>
              模型
              <input
                className="settings-input"
                list="price-model-choices"
                placeholder="模型 id，如 deepseek-chat"
                value={draft.model}
                onChange={(event) => setDraft({ ...draft, model: event.target.value })}
              />
              <datalist id="price-model-choices">
                {modelChoices.map((item) => <option key={`${item.provider}:${item.id}`} value={item.id}>{item.label}</option>)}
              </datalist>
            </label>
            <label>输入<input className="settings-input" inputMode="decimal" value={draft.input} onChange={(event) => setDraft({ ...draft, input: event.target.value })} /></label>
            <label>缓存命中<input className="settings-input" inputMode="decimal" placeholder="留空 = 同输入" value={draft.cacheRead} onChange={(event) => setDraft({ ...draft, cacheRead: event.target.value })} /></label>
            <label>缓存写入<input className="settings-input" inputMode="decimal" placeholder="留空 = 同输入" value={draft.cacheWrite} onChange={(event) => setDraft({ ...draft, cacheWrite: event.target.value })} /></label>
            <label>输出<input className="settings-input" inputMode="decimal" value={draft.output} onChange={(event) => setDraft({ ...draft, output: event.target.value })} /></label>
          </div>
          <p className="credential-note">
            计费时先按模型实际应答的名字找价格，找不到再按请求的名字——服务商返回别名时（如请求 deepseek-chat 应答 deepseek-flash），两个名字都可以定价。
          </p>
          <button type="button" className="primary-button full-button" onClick={() => void save()}>保存价格</button>
        </div>
      )}
      {view}
    </section>
  );
}
